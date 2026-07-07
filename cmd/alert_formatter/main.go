package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/alert_formatter/formatter"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/controller"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/formatters"
)

// config is everything alert_formatter needs. Required fields fail fast at load.
// The embedded formatter.Config carries the service's topic fields; SnapshotTopic and
// PluginDir wire the control plane here in main. Broker is injected post-load.
type config struct {
	services.Common
	formatter.Config
	SnapshotTopic string `env:"KAFKA_TOPIC_FORMATTER_SNAPSHOT"`
	PluginDir     string `env:"FORMATTER_PLUGIN_DIR"`
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

	// Snapshot reader: the sole source of formatter config in the data plane - drives which
	// subprocesses run AND their metadata + rollout (via formatterCfg). No local YAML.
	snapshotReader := controller.NewSnapshotReader(logger.New("formatter-snapshot", "dev"), b.NewBroadcastReader(cfg.SnapshotTopic))
	snapshotReaderSvc := services.NewManagedService("formatter-snapshot-sync", snapshotReader)
	formatterCfg := formatters.NewSnapshotConfig(logger.New("formatter-config", "dev"), snapshotReader)

	formatterPool := formatters.NewPool(formatterCfg, 0)

	pluginMgr := formatters.NewPluginExecutor(logger.New("alert-formatter", "dev"), formatterPool.Sync, cfg.PluginDir, snapshotReader, formatterCfg)
	syncSvc := services.NewManagedService("alert-formatter-sync", pluginMgr)

	formatterSvc := formatter.NewFormatterService(cfg.Config, formatterPool)

	// Readiness: stay out of rotation until the control plane has delivered a snapshot.
	go services.ServeHealth(":8080", snapshotReader.Ready)

	runner := services.New()
	runner.Register(
		snapshotReaderSvc,
		syncSvc,
		formatterSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down alert-formatter")
}
