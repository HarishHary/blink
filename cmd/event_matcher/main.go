package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/event_matcher/matcher"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/matchers"
	matcherconfig "github.com/harishhary/blink/pkg/matchers/config"
	pools "github.com/harishhary/blink/internal/pools"
	matchcatalog "github.com/harishhary/blink/pkg/matchers/pool"
	"github.com/harishhary/blink/pkg/rules/config"
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

	// Rule config watcher (used by the matcher service to look up rules).
	ruleConfigDir := os.Getenv("RULE_CONFIG_DIR")
	if ruleConfigDir == "" {
		log.Fatal("RULE_CONFIG_DIR is required")
	}
	cfgWatcherSvc, err := config.NewWatcher(ruleConfigDir)
	if err != nil {
		log.Fatalf("config watcher: %v", err)
	}

	// Matcher plugin config watcher (YAML sidecars for matcher binaries).
	matcherPluginDir := os.Getenv("MATCHER_PLUGIN_DIR")
	matcherCfgWatcher, err := matcherconfig.NewWatcher(matcherPluginDir)
	if err != nil {
		log.Fatalf("matcher config watcher: %v", err)
	}

	routingTable := pools.NewRoutingTable()
	matcherPool := matchcatalog.NewPool(routingTable, 0)

	syncSvc, err := services.NewPluginSyncService(
		"event-matcher-sync",
		"BLINK-EVENT-MATCHER - SYNC",
		"MATCHER_PLUGIN_DIR",
		func(log *logger.Logger, dir string) plugin.Plugin {
			return matchers.NewManager(log, matcherPool.Sync, dir, matcherCfgWatcher)
		},
	)
	if err != nil {
		log.Fatalf("sync service: %v", err)
	}
	matcherSvc, err := matcher.NewMatcherService(matcherPool, cfgWatcherSvc)
	if err != nil {
		log.Fatalf("matcher service: %v", err)
	}

	runner := services.New()
	runner.Register(
		cfgWatcherSvc,
		matcherCfgWatcher,
		syncSvc,
		matcherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down event-matcher")
}
