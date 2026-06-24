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
	pools "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/tuning_rules"
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
	cfgMgr := tuning_rules.NewTuningRuleConfigWatcher(logger.New("tuning-config", "dev"), pluginDir)
	cfgSvc := services.NewConfigSyncService("tuning-config-sync", "BLINK-RULE-TUNER - CONFIG", cfgMgr)

	routingTable := pools.NewRoutingTable()
	tuningPool := tuning_rules.NewPool(routingTable, 0)

	pluginMgr := tuning_rules.NewTuningRulePluginExecutor(logger.New("rule-tuner", "dev"), tuningPool.Sync, pluginDir, cfgMgr)
	syncSvc, err := services.NewPluginSyncService("rule-tuner-sync", "BLINK-RULE-TUNER - SYNC", pluginMgr)
	if err != nil {
		log.Fatalf("sync service: %v", err)
	}
	tunerSvc, err := tuner.NewTunerService(tuningPool)
	if err != nil {
		log.Fatalf("tuner service: %v", err)
	}

	runner := services.New()
	runner.Register(
		cfgSvc,
		syncSvc,
		tunerSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down rule-tuner")
}
