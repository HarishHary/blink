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

	// The matcher snapshot is the data plane's matcher configuration source.
	matcherSnap := controller.NewSnapshotReader(rootLogger.With("component", "matcher_snapshot"), b.NewBroadcastReader(cfg.MatcherSnapshotTopic))
	matcherSnapSvc := services.NewManagedService("matcher-snapshot-sync", matcherSnap)

	// The rule snapshot is the data plane's rule configuration source.
	ruleSnap := controller.NewSnapshotReader(rootLogger.With("component", "rule_snapshot"), b.NewBroadcastReader(cfg.ExecutorSnapshotTopic))
	ruleSnapSvc := services.NewManagedService("rule-snapshot-sync", ruleSnap)

	// The plugin executor reconciles matcher processes from the matcher snapshot.
	matcherCfg := matchers.NewSnapshotConfig(rootLogger.With("component", "matcher_config"), matcherSnap)
	matcherPool := matchers.NewPool(matcherCfg, 0)
	pluginExecutor := matchers.NewPluginExecutor(rootLogger.With("component", "plugin_executor"), matcherPool.Sync, cfg.MatcherPluginDir, matcherSnap, matcherCfg)
	pluginExecutorSvc := services.NewManagedService("event-matcher-sync", pluginExecutor)

	// The rule catalog selects candidates by event log type.
	ruleCfg := rules.NewSnapshotConfig(rootLogger.With("component", "rule_config"), ruleSnap)
	// Consumption waits for synchronized, non-empty matcher and rule catalogs.
	cfg.Config.ReadyFn = func() bool {
		return matcherSnap.Ready() && ruleSnap.Ready() && len(matcherCfg.Primaries()) > 0 && len(ruleCfg.Primaries()) > 0
	}
	matcherSvc := matcher.NewService(rootLogger.With("component", "service"), cfg.Config, matcherPool, ruleCfg, matcherCfg)

	// Health requires synchronized snapshots but does not require non-empty catalogs.
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
