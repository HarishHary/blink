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

	// Matcher snapshot: the sole source of matcher config in the data plane - drives which
	// subprocesses run AND their metadata + rollout (via matcherCfg). No local YAML.
	matcherSnap := controller.NewSnapshotReader(logger.New("matcher-snapshot", "dev"), b.NewBroadcastReader(cfg.MatcherSnapshotTopic))
	matcherSnapSvc := services.NewManagedService("matcher-snapshot-sync", matcherSnap)
	matcherCfg := matchers.NewSnapshotConfig(logger.New("matcher-config", "dev"), matcherSnap)

	// Rule snapshot: the rollout authority. The matcher derives candidate rules per log_type
	// from this - no RULE_CONFIG_DIR, no duplicate rule-YAML mount.
	ruleSnap := controller.NewSnapshotReader(logger.New("rule-snapshot", "dev"), b.NewBroadcastReader(cfg.RuleSnapshotTopic))
	ruleSnapSvc := services.NewManagedService("rule-snapshot-sync", ruleSnap)
	ruleCfg := rules.NewSnapshotConfig(logger.New("rule-config", "dev"), ruleSnap)

	matcherPool := matchers.NewPool(matcherCfg, 0)

	pluginMgr := matchers.NewPluginExecutor(logger.New("event-matcher", "dev"), matcherPool.Sync, cfg.MatcherPluginDir, matcherSnap, matcherCfg)
	syncSvc := services.NewManagedService("event-matcher-sync", pluginMgr)

	matcherSvc := matcher.NewService(cfg.Config, matcherPool, ruleCfg)

	// Readiness: ready only once BOTH control-plane inputs have arrived - the matcher
	// plugins to run and the rule rollout to apply.
	ready := func() bool { return matcherSnap.Ready() && ruleSnap.Ready() }
	go services.ServeHealth(":8080", ready)

	runner := services.New()
	runner.Register(
		matcherSnapSvc,
		ruleSnapSvc,
		syncSvc,
		matcherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down event-matcher")
}
