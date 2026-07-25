package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/rule_executor/executor"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/controller"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/rules"
)

type config struct {
	services.Common
	executor.Config
	ExecutorSnapshotTopic string `env:"KAFKA_TOPIC_EXECUTOR_SNAPSHOT"`
	RulePluginDir         string `env:"RULE_PLUGIN_DIR"`
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
	rootLogger := logger.New("rule-executor", cfg.Env)

	// The rule snapshot is the data plane's rule configuration source.
	ruleSnap := controller.NewSnapshotReader(rootLogger.With("component", "rule_snapshot"), b.NewBroadcastReader(cfg.ExecutorSnapshotTopic))
	ruleSnapSvc := services.NewManagedService("rule-snapshot-sync", ruleSnap)

	// The plugin executor reconciles rule processes from the rule snapshot.
	ruleCfg := rules.NewSnapshotConfig(rootLogger.With("component", "rule_config"), ruleSnap)
	rulePool := rules.NewPool(rootLogger.With("component", "rule_pool"), ruleCfg, 0)
	pluginExecutor := rules.NewPluginExecutor(rootLogger.With("component", "plugin_executor"), rulePool.Sync, cfg.RulePluginDir, ruleSnap, ruleCfg)
	pluginExecutorSvc := services.NewManagedService("rule-executor-sync", pluginExecutor)

	// The executor consumes routed events and publishes alerts to the merger.
	cfg.Config.ReadyFn = func() bool { return ruleSnap.Ready() && len(ruleCfg.Primaries()) > 0 }
	executorSvc := executor.NewService(rootLogger.With("component", "service"), cfg.Config, rulePool, ruleCfg)

	go services.ServeHealth(rootLogger.With("component", "health"), ":8080", func() bool { return ruleSnap.Ready() })

	runner := services.New(rootLogger.With("component", "runner"))
	runner.Register(ruleSnapSvc, pluginExecutorSvc, executorSvc)
	runner.Run(ctx)
	log.Println("Shutting down rule-executor")
}
