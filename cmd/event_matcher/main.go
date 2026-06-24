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
	pools "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
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

	// Rule config manager (used by the matcher service to look up rules).
	ruleConfigDir := os.Getenv("RULE_CONFIG_DIR")
	if ruleConfigDir == "" {
		log.Fatal("RULE_CONFIG_DIR is required")
	}
	ruleCfgMgr := rules.NewRuleConfigWatcher(logger.New("rule-config", "dev"), ruleConfigDir)
	ruleCfgSvc := services.NewConfigSyncService("rule-config-sync", "BLINK-EVENT-MATCHER - RULE-CONFIG", ruleCfgMgr)

	// Matcher plugin config manager (YAML sidecars for matcher binaries).
	matcherPluginDir := os.Getenv("MATCHER_PLUGIN_DIR")
	matcherCfgMgr := matchers.NewMatcherConfigWatcher(logger.New("matcher-config", "dev"), matcherPluginDir)
	matcherCfgSvc := services.NewConfigSyncService("matcher-config-sync", "BLINK-EVENT-MATCHER - MATCHER-CONFIG", matcherCfgMgr)

	routingTable := pools.NewRoutingTable()
	matcherPool := matchers.NewPool(routingTable, 0)

	pluginMgr := matchers.NewMatcherPluginExecutor(logger.New("event-matcher", "dev"), matcherPool.Sync, matcherPluginDir, matcherCfgMgr)
	syncSvc, err := services.NewPluginSyncService("event-matcher-sync", "BLINK-EVENT-MATCHER - SYNC", pluginMgr)
	if err != nil {
		log.Fatalf("sync service: %v", err)
	}
	matcherSvc, err := matcher.NewMatcherService(matcherPool, ruleCfgMgr)
	if err != nil {
		log.Fatalf("matcher service: %v", err)
	}

	runner := services.New()
	runner.Register(
		ruleCfgSvc,
		matcherCfgSvc,
		syncSvc,
		matcherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down event-matcher")
}
