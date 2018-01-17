package main

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	logging "github.com/op/go-logging"
)

// Version is the released program version
const Version = "0.02"
const userAgent = "gostats/" + Version

// config file structures
type tomlConfig struct {
	Global     globalConfig
	Clusters   []clusterconf
	StatGroups []statgroupconf
}

type globalConfig struct {
	Processor        string   `toml:"stats_processor"`
	ProcessorArgs    []string `toml:"stats_processor_args"`
	ActiveStatGroups []string `toml:"active_stat_groups"`
	MinUpdateInvtl   int      `toml:"min_update_interval_override"`
}

type clusterconf struct {
	Hostname string
	Username string
	Password string
	SSLCheck bool `toml:"verify-ssl"`
}

type statgroupconf struct {
	Name        string
	UpdateIntvl string `toml:"update_interval"`
	Stats       []string
}

type statconf struct {
	statGroups       map[string][]string
	activeStatGroups []string
	stats            []string
}

// parsed/populated stat structures
type stat struct {
	key      string
	units    string
	datatype string // JSON "type"
	// add enum for this
	aggType     string
	updateIntvl time.Duration
}

type statgroup struct {
	name        string
	updateIntvl time.Duration
	stats       []string
}

var log = logging.MustGetLogger("gostats")

type loglevel logging.Level

var logFileName = flag.String("logfile", "./gostats.log", "pathname of log file")
var logLevel = loglevel(logging.NOTICE)
var configFileName = flag.String("config-file", "idic.toml", "pathname of config file")

// debugging flags
var checkStatReturn = flag.Bool("check-stat-return",
	false,
	"Verify that the api returns results for every stat requested")

func (l *loglevel) String() string {
	level := logging.Level(*l)
	return level.String()
}

func (l *loglevel) Set(value string) error {
	level, err := logging.LogLevel(value)
	if err != nil {
		return err
	}
	*l = loglevel(level)
	return nil
}

func init() {
	// tie log-level variable into flag parsing
	flag.Var(&logLevel,
		"loglevel",
		"default log level [CRITICAL|ERROR|WARNING|NOTICE|INFO|DEBUG]")
}

func main() {
	// parse command line
	flag.Parse()

	// set up logging
	setupLogging()

	// announce ourselves
	log.Notice("Starting gostats")

	// read in our config
	conf := mustReadConfig()
	log.Info("Successfully read config file")

	// Determine which stats to poll
	log.Info("Parsing stat groups and stats")
	// XXX need to pull groups for update information
	sc := parseStatConfig(conf)
	log.Infof("Parsed stats; %d stats will be collected", len(sc.stats))

	// start collecting from each defined cluster
	var wg sync.WaitGroup
	wg.Add(len(conf.Clusters))
	for _, cl := range conf.Clusters {
		go func(cl clusterconf) {
			log.Infof("starting collect for cluster %s", cl.Hostname)
			defer wg.Done()
			statsloop(cl, conf.Global, sc.stats)
		}(cl)
	}
	wg.Wait()
}

func mustReadConfig() tomlConfig {
	var conf tomlConfig
	_, err := toml.DecodeFile(*configFileName, &conf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: unable to read config file %s, exiting\n", os.Args[0], *configFileName)
		log.Fatal(err)
	}
	return conf
}

func parseStatConfig(conf tomlConfig) statconf {
	statgroups := make(map[string][]string)
	for _, sg := range conf.StatGroups {
		statgroups[sg.Name] = sg.Stats
	}
	// validate active groups
	asg := []string{}
	for _, group := range conf.Global.ActiveStatGroups {
		if _, ok := statgroups[group]; !ok {
			log.Warningf("Active stat group %q not found - removing\n", group)
			continue
		}
		asg = append(asg, group)
	}
	// dedup stats using allstats as a set
	allstats := make(map[string]bool)
	for _, sg := range asg {
		for _, stat := range statgroups[sg] {
			allstats[stat] = true
		}
	}
	stats := []string{}
	for stat := range allstats {
		stats = append(stats, stat)
	}
	return statconf{statgroups, asg, stats}
}

func setupLogging() {
	f, err := os.OpenFile(*logFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gostats: unable to open log file %s for output - %s", *logFileName, err)
		os.Exit(2)
	}
	backend := logging.NewLogBackend(f, "", 0)
	var format = logging.MustStringFormatter(
		`%{time:2006-01-02T15:04:05Z07:00} %{shortfile} %{level} %{message}`,
	)
	backendFormatter := logging.NewBackendFormatter(backend, format)
	backendLeveled := logging.AddModuleLevel(backendFormatter)
	backendLeveled.SetLevel(logging.Level(logLevel), "")
	logging.SetBackend(backendLeveled)
}

func statsloop(cluster clusterconf, gc globalConfig, stats []string) {
	var err error
	var ss DBWriter
	// Connect to the cluster
	c := &Cluster{
		AuthInfo: AuthInfo{
			Username: cluster.Username,
			Password: cluster.Password,
		},
		Hostname:  cluster.Hostname,
		Port:      8080,
		VerifySSL: cluster.SSLCheck,
	}
	if err = c.Connect(); err != nil {
		log.Errorf("Connection to cluster %s failed: %v", c.Hostname, err)
		return
	}
	log.Infof("Connected to cluster %s, version %s", c.ClusterName, c.OSVersion)

	// Configure/initialize backend database writer
	ss, err = getDBWriter(gc.Processor)
	if err != nil {
		log.Error(err)
		return
	}
	err = ss.Init(c.ClusterName, gc.ProcessorArgs)
	if err != nil {
		log.Errorf("Unable to initialize %s plugin: %v", gc.Processor, err)
		return
	}

	// loop collecting and pushing stats
	readFailCount := 0
	const readFailLimit = 30
	for {
		nextTime := time.Now().Add(30 * time.Second)
		// Collect one set of stats
		log.Infof("cluster %s start collecting stats", c.ClusterName)
		sr, err := c.GetStats(stats)
		if err != nil {
			readFailCount++
			if readFailCount >= readFailLimit {
				log.Errorf("Unable to collect stats from %s after %d tries, giving up", c.ClusterName, readFailLimit)
				return
			}
			log.Errorf("Failed to retrieve stats for cluster %q: %v\n", c.ClusterName, err)
			log.Errorf("Retry #%d in 1 minute", readFailCount)
			time.Sleep(time.Minute)
			continue
		}
		readFailCount = 0
		if *checkStatReturn {
			verifyStatReturn(c.ClusterName, stats, sr)
		}

		log.Infof("cluster %s start writing stats to back end", c.ClusterName)
		err = ss.WriteStats(sr)
		if err != nil {
			// XXX maybe implement backoff here?
			log.Errorf("Failed to write stats to database: %s", err)
			return
		}
		sleepTime := time.Until(nextTime)
		log.Infof("cluster %s sleeping for %v", c.ClusterName, sleepTime)
		time.Sleep(sleepTime)
	}
}

// return a DBWriter for the given backend name
func getDBWriter(sp string) (DBWriter, error) {
	if sp != "influxdb_plugin" {
		return nil, fmt.Errorf("unsupported backend plugin %s", sp)
	}
	return GetInfluxDBWriter(), nil
}

func verifyStatReturn(cluster string, stats []string, sr []StatResult) {
	resultNames := make(map[string]bool)
	missing := []string{}
	for _, result := range sr {
		resultNames[result.Key] = true
	}
	for _, stat := range stats {
		if !resultNames[stat] {
			missing = append(missing, stat)
		}
	}
	if len(missing) != 0 {
		log.Errorf("Stats collection for cluster %s failed to collect the following stats: %v", cluster, missing)
	}
}
