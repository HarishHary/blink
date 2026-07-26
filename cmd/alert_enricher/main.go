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

// config contains the environment-loaded alert-enricher settings.
type config struct {
	services.Common
	enricher.Config
	EnricherSnapshotTopic string `env:"KAFKA_TOPIC_ENRICHER_SNAPSHOT"`
	EnrichmentPluginDir   string `env:"ENRICHER_PLUGIN_DIR"`
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
	rootLogger := logger.New("alert-enricher", cfg.Env)

	// The snapshot drives both enrichment metadata and subprocess lifecycle.
	enrichmentSnap := controller.NewSnapshotReader(rootLogger.With("component", "enrichment_snapshot"), b.NewBroadcastReader(cfg.EnricherSnapshotTopic))
	enrichmentSnapSvc := services.NewManagedService("enrichment-snapshot-sync", enrichmentSnap)
	enrichmentCfg := enrichments.NewSnapshotConfig(rootLogger.With("component", "enrichment_config"), enrichmentSnap)

	enricherPool := enrichments.NewPool(rootLogger.With("component", "enrichment_pool"), enrichmentCfg, 0)

	pluginExecutor := enrichments.NewPluginExecutor(rootLogger.With("component", "plugin_executor"), enricherPool.Sync, cfg.EnrichmentPluginDir, enrichmentSnap, enrichmentCfg)
	pluginExecutorSvc := services.NewManagedService("alert-enricher-sync", pluginExecutor)

	cfg.Config.ReadyFn = func() bool {
		return enrichmentSnap.Ready() && len(enrichmentCfg.Primaries()) > 0
	}
	enricherSvc := enricher.NewService(rootLogger.With("component", "service"), cfg.Config, enricherPool, enrichmentCfg)

	// Health only requires snapshot catch-up; consumption also requires a non-empty primary catalog.
	go services.ServeHealth(rootLogger.With("component", "health"), ":8080", enrichmentSnap.Ready)

	runner := services.New(rootLogger.With("component", "runner"))
	runner.Register(
		enrichmentSnapSvc,
		pluginExecutorSvc,
		enricherSvc,
	)
	runner.Run(ctx)
	log.Println("Shutting down alert-enricher")
}
