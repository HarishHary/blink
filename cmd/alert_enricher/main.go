package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/cmd/alert_enricher/enricher"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/controller"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/enrichments"
)

// config is everything alert_enricher needs. Required fields fail fast at load.
// The embedded enricher.Config carries the service's topic fields; SnapshotTopic and
// PluginDir wire the control plane here in main. Broker is injected post-load.
type config struct {
	services.Common
	enricher.Config
	SnapshotTopic string `env:"KAFKA_TOPIC_ENRICHMENT_SNAPSHOT"`
	PluginDir     string `env:"ENRICHER_PLUGIN_DIR"`
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

	// Snapshot reader: the sole source of enrichment config in the data plane - drives which
	// subprocesses run AND their metadata + rollout (via enrichmentCfg). No local YAML.
	snapshotReader := controller.NewSnapshotReader(logger.New("enrichment-snapshot", "dev"), b.NewBroadcastReader(cfg.SnapshotTopic))
	snapshotReaderSvc := services.NewManagedService("enrichment-snapshot-sync", snapshotReader)
	enrichmentCfg := enrichments.NewSnapshotConfig(logger.New("enrichment-config", "dev"), snapshotReader)

	enricherPool := enrichments.NewPool(enrichmentCfg, 0)

	pluginMgr := enrichments.NewPluginExecutor(logger.New("enricher", "dev"), enricherPool.Sync, cfg.PluginDir, snapshotReader, enrichmentCfg)
	syncSvc := services.NewManagedService("alert-enricher-sync", pluginMgr)

	enricherSvc := enricher.NewEnricherService(cfg.Config, enricherPool)

	// Readiness: stay out of rotation until the control plane has delivered a snapshot.
	go services.ServeHealth(":8080", snapshotReader.Ready)

	runner := services.New()
	runner.Register(
		snapshotReaderSvc,
		syncSvc,
		enricherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down alert-enricher")
}
