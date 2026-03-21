package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/rule_executor/executor"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/pluginmgr"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/rules/config"
	rulecatalog "github.com/harishhary/blink/pkg/rules/pool"
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

	// RULE_PLUGIN_DIR contains both the rule binaries and their .yaml sidecars.
	// The config watcher must start before the rule manager so YAML configs are
	// available when binaries are first discovered.
	rulePluginDir := os.Getenv("RULE_PLUGIN_DIR")
	if rulePluginDir == "" {
		log.Fatal("RULE_PLUGIN_DIR is required")
	}
	cfgWatcher, err := config.NewWatcher(rulePluginDir)
	if err != nil {
		log.Fatalf("config watcher: %v", err)
	}

	rulePool := rulecatalog.NewPool(cfgWatcher, 0)

	syncSvc, err := services.NewPluginSyncService(
		"rule-executor-sync",
		"BLINK-RULE-EXECUTOR - SYNC",
		"RULE_PLUGIN_DIR",
		func(log *logger.Logger, dir string) pluginmgr.Plugin {
			return rules.NewManager(log, rulePool.Sync, dir, cfgWatcher)
		},
	)
	if err != nil {
		log.Fatalf("sync service: %v", err)
	}

	executorSvc, err := executor.NewExecutorService(rulePool, cfgWatcher)
	if err != nil {
		log.Fatalf("executor service: %v", err)
	}

	runner := services.New()
	runner.Register(
		cfgWatcher,
		syncSvc,
		executorSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down rule-executor")
}
