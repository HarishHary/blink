package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/event_matcher/matcher"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/controller"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
)

// config is everything event_matcher needs. See docs/services/event_matcher.md.
type config struct {
	services.Common
	matcher.Config
	MatcherSnapshotTopic  string `env:"KAFKA_TOPIC_MATCHER_SNAPSHOT"`
	ExecutorSnapshotTopic string `env:"KAFKA_TOPIC_EXECUTOR_SNAPSHOT"`
	MatcherPluginDir      string `env:"MATCHER_PLUGIN_DIR"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	cfg.Config.Broker = brokers.NewKafkaBroker(cfg.Kafka)
	b := cfg.Config.Broker
	rootLogger := logger.New("event-matcher", cfg.Env)

	// Matcher snapshot: the sole source of matcher config in the data plane
	matcherSnap := controller.NewSnapshotReader(rootLogger.With("component", "matcher_snapshot"), b.NewBroadcastReader(cfg.MatcherSnapshotTopic))
	matcherSnapSvc := services.NewManagedService("matcher-snapshot-sync", matcherSnap)

	// Rule snapshot: the sole source of rule config in the data plane
	ruleSnap := controller.NewSnapshotReader(rootLogger.With("component", "rule_snapshot"), b.NewBroadcastReader(cfg.ExecutorSnapshotTopic))
	ruleSnapSvc := services.NewManagedService("rule-snapshot-sync", ruleSnap)

	// Matcher Plugin Executor: runs the matcher plugins based on the matcher snapshot
	matcherCfg := matchers.NewSnapshotConfig(rootLogger.With("component", "matcher_config"), matcherSnap)
	matcherPool := matchers.NewPool(matcherCfg, 0)
	pluginExecutor := matchers.NewPluginExecutor(rootLogger.With("component", "plugin_executor"), matcherPool.Sync, cfg.MatcherPluginDir, matcherSnap, matcherCfg)
	pluginExecutorSvc := services.NewManagedService("event-matcher-sync", pluginExecutor)

	// Rule Config: to select rules for the event matcher based on the rule log type
	ruleCfg := rules.NewSnapshotConfig(rootLogger.With("component", "rule_config"), ruleSnap)
	// Gate consumption until both control-plane snapshots have caught up and
	// each has published at least one effective primary configuration.
	readyToConsume := snapshotCatalogsReady(matcherSnap.Ready, ruleSnap.Ready, matcherCfg, ruleCfg)
	cfg.Config.Ready = readyToConsume
	matcherSvc := matcher.NewService(rootLogger.With("component", "service"), cfg.Config, matcherPool, ruleCfg, matcherCfg)

	// The pod is ready once both control-plane inputs are synchronized. Consumption
	// remains gated above until there are matcher and rule catalogs to process with.
	go services.ServeHealth(":8080", func() bool { return matcherSnap.Ready() && ruleSnap.Ready() })

	runner := services.New()
	runner.Register(
		matcherSnapSvc,
		ruleSnapSvc,
		pluginExecutorSvc,
		matcherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down event-matcher")
}

func snapshotCatalogsReady(matcherReady, ruleReady func() bool, matcherCfg *matchers.SnapshotConfig, ruleCfg *rules.SnapshotConfig) func() bool {
	return func() bool {
		return matcherReady() && ruleReady() && len(matcherCfg.Primaries()) > 0 && len(ruleCfg.Primaries()) > 0
	}
}
