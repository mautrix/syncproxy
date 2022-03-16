// mautrix-syncproxy - A /sync proxy for encrypted Matrix appservices.
// Copyright (C) 2021 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "maunium.net/go/maulogger/v2"
)

type Config struct {
	ListenAddress     string `yaml:"listen_address"`
	DatabaseURL       string `yaml:"database_url"`
	HomeserverURL     string `yaml:"homeserver_url"`
	SharedSecret      string `yaml:"shared_secret"`
	ExpectSynchronous bool   `yaml:"expect_synchronous"`
	Debug             bool   `yaml:"debug"`

	DatabaseOpts DatabaseOpts `yaml:"database_opts"`
}

var cfg Config
var db *Database

func getIntEnv(key string, defVal int) int {
	strVal, ok := os.LookupEnv(key)
	if !ok {
		return defVal
	}
	val, err := strconv.Atoi(strVal)
	if err != nil {
		return defVal
	}
	return val
}

func readConfig() {
	cfg.ListenAddress = os.Getenv("LISTEN_ADDRESS")
	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	cfg.DatabaseOpts.MaxOpenConns = getIntEnv("DATABASE_MAX_OPEN_CONNS", 4)
	cfg.DatabaseOpts.MaxIdleConns = getIntEnv("DATABASE_MAX_IDLE_CONNS", 2)
	cfg.HomeserverURL = os.Getenv("HOMESERVER_URL")
	cfg.SharedSecret = os.Getenv("SHARED_SECRET")
	cfg.ExpectSynchronous = len(os.Getenv("EXPECT_SYNCHRONOUS")) > 0
	cfg.Debug = len(os.Getenv("DEBUG")) > 0

	if len(cfg.ListenAddress) == 0 {
		log.Fatalln("LISTEN_ADDRESS environment variable is not set")
	} else if len(cfg.DatabaseURL) == 0 {
		log.Fatalln("DATABASE_URL environment variable is not set")
	} else if len(cfg.HomeserverURL) == 0 {
		log.Fatalln("HOMESERVER_URL environment variable is not set")
	} else if len(cfg.SharedSecret) == 0 {
		log.Fatalln("SHARED_SECRET environment variable is not set")
	} else {
		return
	}

	os.Exit(2)
}

func main() {
	log.DefaultLogger.TimeFormat = "Jan _2, 2006 15:04:05"
	readConfig()
	if cfg.Debug {
		log.DefaultLogger.PrintLevel = log.LevelDebug.Severity
	}
	if localDB, err := Connect(cfg.DatabaseURL, cfg.DatabaseOpts); err != nil {
		log.Fatalln("Failed to connect to database:", err)
		os.Exit(3)
	} else {
		db = localDB
	}

	if err := db.Upgrade(); err != nil {
		log.Fatalln("Failed to upgrade database:", err)
		os.Exit(4)
	} else if err = LoadTargets(); err != nil {
		log.Fatalln("Failed to load old targets from database:", err)
		os.Exit(5)
	}

	log.Infoln("Starting old active targets")
	startedCount := 0
	for _, target := range targets {
		if target.Active {
			go target.Start()
			startedCount += 1
		}
	}
	log.Infofln("Started %d active targets out of %d total old targets", startedCount, len(targets))

	router := mux.NewRouter()
	router.HandleFunc("/_matrix/client/unstable/fi.mau.syncproxy/{appserviceID}", startSync).Methods(http.MethodPut, http.MethodDelete)
	router.Handle("/metrics", promhttp.Handler())
	server := &http.Server{
		Addr:    cfg.ListenAddress,
		Handler: router,
	}
	go func() {
		log.Infoln("Starting to listen on", cfg.ListenAddress)
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			log.Fatalln("Error in listener:", err)
			os.Exit(6)
		}
	}()

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Errorln("Failed to close server:", err)
	}
}
