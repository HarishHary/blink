package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/rule_tuner/tuner"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/tuning_rules"
	tuningconfig "github.com/harishhary/blink/pkg/tuning_rules/config"
	pools "github.com/harishhary/blink/internal/pools"
	tuningcatalog "github.com/harishhary/blink/pkg/tuning_rules/pool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		http.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pluginDir := os.Getenv("TUNER_PLUGIN_DIR")
	cfgWatcher, err := tuningconfig.NewWatcher(pluginDir)
	if err != nil {
		log.Fatalf("tuning config watcher: %v", err)
	}

	routingTable := pools.NewRoutingTable()
	tuningPool := tuningcatalog.NewPool(routingTable, 0)

	syncSvc, err := services.NewPluginSyncService(
		"rule-tuner-sync",
		"BLINK-RULE-TUNER - SYNC",
		"TUNER_PLUGIN_DIR",
		func(log *logger.Logger, dir string) plugin.Plugin {
			return tuning_rules.NewManager(log, tuningPool.Sync, dir, cfgWatcher)
		},
	)
	if err != nil {
		log.Fatalf("sync service: %v", err)
	}
	tunerSvc, err := tuner.NewTunerService(tuningPool)
	if err != nil {
		log.Fatalf("tuner service: %v", err)
	}

	runner := services.New()
	runner.Register(
		cfgWatcher,
		syncSvc,
		tunerSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down rule-tuner")
}
