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

// config is everything event_matcher needs. Required fields fail fast at load.
// Rule rollout comes from the rule snapshot (RuleSnapshotTopic) - there is no RULE_CONFIG_DIR.
// The embedded matcher.Config carries the service's topic fields; the snapshot topics and
// plugin dir wire the control plane here in main. Broker is injected post-load.
type config struct {
	services.Common
	matcher.Config
	MatcherSnapshotTopic string `env:"KAFKA_TOPIC_MATCHER_SNAPSHOT"`
	RuleSnapshotTopic    string `env:"KAFKA_TOPIC_RULE_SNAPSHOT"`
	MatcherPluginDir     string `env:"MATCHER_PLUGIN_DIR"`
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
	ruleSnap := controller.NewSnapshotReader(rootLogger.With("component", "rule_snapshot"), b.NewBroadcastReader(cfg.RuleSnapshotTopic))
	ruleSnapSvc := services.NewManagedService("rule-snapshot-sync", ruleSnap)

	// Matcher Plugin Executor: runs the matcher plugins based on the matcher snapshot
	matcherCfg := matchers.NewSnapshotConfig(rootLogger.With("component", "matcher_config"), matcherSnap)
	matcherPool := matchers.NewPool(matcherCfg, 0)
	pluginExector := matchers.NewPluginExecutor(rootLogger.With("component", "plugin_executor"), matcherPool.Sync, cfg.MatcherPluginDir, matcherSnap, matcherCfg)
	pluginExectorSvc := services.NewManagedService("event-matcher-sync", pluginExector)

	// Rule Config: to select rules for the event matcher based on the rule log type
	ruleCfg := rules.NewSnapshotConfig(rootLogger.With("component", "rule_config"), ruleSnap)
	event_matcherSvc := matcher.NewService(rootLogger.With("component", "service"), cfg.Config, matcherPool, ruleCfg)

	// Readiness: ready only once BOTH control-plane inputs have arrived
	// FIXME: Wait for the pluginExecutor to be ready as well
	ready := func() bool { return matcherSnap.Ready() && ruleSnap.Ready() }
	go services.ServeHealth(":8080", ready)

	runner := services.New()
	runner.Register(
		matcherSnapSvc,
		ruleSnapSvc,
		pluginExectorSvc,
		event_matcherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down event-matcher")
}
