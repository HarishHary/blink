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

// config is everything rule_tuner needs. Required fields fail fast at load.
// The embedded tuner.Config carries the service's topic fields; SnapshotTopic and
// PluginDir wire the control plane here in main. Broker is injected post-load.
type config struct {
	services.Common
	tuner.Config
	SnapshotTopic string `env:"KAFKA_TOPIC_TUNING_SNAPSHOT"`
	PluginDir     string `env:"TUNER_PLUGIN_DIR"`
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

	// Snapshot reader: the sole source of tuning-rule config in the data plane - drives which
	// subprocesses run AND their metadata + rolloutw (via tuningCfg). No local YAML.
	snapshotReader := controller.NewSnapshotReader(logger.New("tuning-snapshot", "dev"), b.NewBroadcastReader(cfg.SnapshotTopic))
	snapshotReaderSvc := services.NewManagedService("tuning-snapshot-sync", snapshotReader)
	tuningCfg := tuning_rules.NewSnapshotConfig(logger.New("tuning-config", "dev"), snapshotReader)

	tuningPool := tuning_rules.NewPool(tuningCfg, 0)

	pluginMgr := tuning_rules.NewPluginExecutor(logger.New("rule-tuner", "dev"), tuningPool.Sync, cfg.PluginDir, snapshotReader, tuningCfg)
	syncSvc := services.NewManagedService("rule-tuner-sync", pluginMgr)

	tunerSvc := tuner.NewTunerService(cfg.Config, tuningPool)

	// Readiness: stay out of rotation until the control plane has delivered a snapshot.
	go services.ServeHealth(":8080", snapshotReader.Ready)

	runner := services.New()
	runner.Register(
		snapshotReaderSvc,
		syncSvc,
		tunerSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down rule-tuner")
}
