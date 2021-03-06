// Copyright 2015 Reborndb Org. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	docopt "github.com/docopt/docopt-go"
	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/ngaut/zkhelper"
	"github.com/reborndb/reborn/pkg/env"
	"github.com/reborndb/reborn/pkg/utils"
)

var (
	cpus            = 2
	addr            = "127.0.0.1:39000"
	dataDir         = "./var/data"
	logDir          = "./var/log"
	configFile      = "config.ini"
	qdbConfigFile   = "" // like "qdb.toml"
	redisConfigFile = "" // like "redis.conf"

	agentID    string
	globalEnv  env.Env
	globalConn zkhelper.Conn

	haMaxRetryNum = 3
	haRetryDelay  = 1
)

var usage = `usage: reborn-agent [options]

options:
    -c <config_file>               base environment config for reborn config and proxy
    -L <log_file>                  set output log file, default is stdout
    --cpu=<cpu_num>                num of cpu cores that reborn can use
    --data-dir=<data_dir>          directory to store important data
    --exec-path=<exec_path>        execution path which we can find reborn-* cmds
    --ha                           start HA for store monitor and failover
    --ha-max-retry-num=<num>       maximum retry number for checking store
    --ha-retry-delay=<n_seconds>   wait n seconds for next check
    --http-addr=<http_addr>        agent http listen address, example: 127.0.0.1:39000
    --log-dir=<app_log_dir>        directory to store log
    --log-level=<loglevel>         set log level: info, warn, error, debug [default: info]
    --qdb-config=<qdb_config>      base qdb config
    --redis-config=<redis_config>  base redis config for reborn-server
`

func getStringArg(args map[string]interface{}, key string) string {
	if v := args[key]; v != nil {
		return v.(string)
	} else {
		return ""
	}
}

func setIntArgFromOpt(dest *int, args map[string]interface{}, key string) {
	v := getStringArg(args, key)
	if len(v) == 0 {
		return
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("parse int arg err %v", err)
		return
	}

	*dest = n
}

func setStringFromOpt(dest *string, args map[string]interface{}, key string) {
	v := getStringArg(args, key)
	if len(v) > 0 {
		*dest = v
	}
}

func resetAbsPath(dest *string) {
	if len(*dest) == 0 {
		return
	}

	var err error
	*dest, err = filepath.Abs(*dest)
	if err != nil {
		log.Fatal(err)
	}
}

func fatal(msg interface{}) {
	if globalConn != nil {
		globalConn.Close()
	}

	// cleanup
	switch msg.(type) {
	case string:
		log.Fatal(msg)
	case error:
		log.Fatal(errors.ErrorStack(msg.(error)))
	}
}

func main() {
	log.SetLevelByString("info")

	args, err := docopt.Parse(usage, nil, true, "reborn agent v0.1", true)
	if err != nil {
		log.Fatal(err)
	}

	setStringFromOpt(&configFile, args, "-c")
	resetAbsPath(&configFile)

	cfg, err := utils.InitConfigFromFile(configFile)
	if err != nil {
		fatal(err)
	}

	globalEnv = env.LoadRebornEnv(cfg)
	globalConn, err = globalEnv.NewCoordConn()
	if err != nil {
		fatal(err)
	}

	setStringFromOpt(&addr, args, "--http-addr")

	agentID = genProcID()

	if err := addAgent(&agentInfo{
		ID:   agentID,
		Addr: addr,
		PID:  os.Getpid(),
	}); err != nil {
		fatal(err)
	}

	setStringFromOpt(&qdbConfigFile, args, "--qdb-config")
	resetAbsPath(&qdbConfigFile)

	setStringFromOpt(&redisConfigFile, args, "--redis-config")
	resetAbsPath(&redisConfigFile)

	if v := getStringArg(args, "--exec-path"); len(v) > 0 {
		path := os.ExpandEnv(fmt.Sprintf("${PATH}:%s", v))
		os.Setenv("PATH", path)
	}

	// set data dir
	setStringFromOpt(&dataDir, args, "--data-dir")
	resetAbsPath(&dataDir)

	os.MkdirAll(baseProcDataDir(), 0755)

	// set app log dir
	setStringFromOpt(&logDir, args, "--log-dir")
	resetAbsPath(&logDir)

	os.MkdirAll(baseProcLogDir(), 0755)

	// set app trash log dir
	os.MkdirAll(baseTrashLogDir(), 0755)

	// set output log file
	if v := getStringArg(args, "-L"); len(v) > 0 {
		log.SetHighlighting(false)
		log.SetOutputByName(v)
	}

	// set log level
	if v := getStringArg(args, "--log-level"); len(v) > 0 {
		log.SetLevelByString(v)
	}

	// set cpu
	if v := getStringArg(args, "--cpu"); len(v) > 0 {
		cpus, err = strconv.Atoi(v)
		if err != nil {
			fatal(err)
		}
	}

	runtime.GOMAXPROCS(cpus)

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, os.Interrupt, os.Kill)
	go func() {
		<-c

		fatal("ctrl-c or SIGTERM found, exit")
	}()

	if args["--ha"].(bool) {
		setIntArgFromOpt(&haMaxRetryNum, args, "--ha-max-retry-num")
		setIntArgFromOpt(&haRetryDelay, args, "--ha-retry-delay")

		go startHA()
	}

	err = loadSavedProcs()
	if err != nil {
		log.Fatalf("restart agent using last saved processes err: %v", err)
	}

	clearProcFiles()

	go runCheckProcs()

	log.Infof("listening %s", addr)
	runHTTPServer()
}
