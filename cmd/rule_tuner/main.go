package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/rule_tuner/tuner"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/controller"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/tuning_rules"
)

// config contains the environment-loaded rule-tuner settings.
type config struct {
	services.Common
	tuner.Config
	TunerSnapshotTopic string `env:"KAFKA_TOPIC_TUNER_SNAPSHOT"`
	TuningPluginDir    string `env:"TUNER_PLUGIN_DIR"`
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
	rootLogger := logger.New("rule-tuner", cfg.Env)

	// The snapshot drives both tuning-rule metadata and subprocess lifecycle.
	tuningSnap := controller.NewSnapshotReader(rootLogger.With("component", "tuning_snapshot"), b.NewBroadcastReader(cfg.TunerSnapshotTopic))
	tuningSnapSvc := services.NewManagedService("tuning-snapshot-sync", tuningSnap)
	tuningCfg := tuning_rules.NewSnapshotConfig(rootLogger.With("component", "tuning_config"), tuningSnap)

	tuningPool := tuning_rules.NewPool(tuningCfg, 0)

	pluginExecutor := tuning_rules.NewPluginExecutor(rootLogger.With("component", "plugin_executor"), tuningPool.Sync, cfg.TuningPluginDir, tuningSnap, tuningCfg)
	pluginExecutorSvc := services.NewManagedService("rule-tuner-sync", pluginExecutor)

	cfg.Config.ReadyFn = func() bool {
		return tuningSnap.Ready() && len(tuningCfg.Primaries()) > 0
	}
	tunerSvc := tuner.NewService(rootLogger.With("component", "service"), cfg.Config, tuningPool, tuningCfg)

	// Health only requires snapshot catch-up; consumption also requires a non-empty primary catalog.
	go services.ServeHealth(":8080", tuningSnap.Ready)

	runner := services.New()
	runner.Register(
		tuningSnapSvc,
		pluginExecutorSvc,
		tunerSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down rule-tuner")
}
