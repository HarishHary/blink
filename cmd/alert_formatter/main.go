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

// config contains the environment-loaded alert-formatter settings.
type config struct {
	services.Common
	formatter.Config
	FormatterSnapshotTopic string `env:"KAFKA_TOPIC_FORMATTER_SNAPSHOT"`
	FormatterPluginDir     string `env:"FORMATTER_PLUGIN_DIR"`
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
	rootLogger := logger.New("alert-formatter", cfg.Env)

	// The snapshot drives both formatter metadata and subprocess lifecycle.
	formatterSnap := controller.NewSnapshotReader(rootLogger.With("component", "formatter_snapshot"), b.NewBroadcastReader(cfg.FormatterSnapshotTopic))
	formatterSnapSvc := services.NewManagedService("formatter-snapshot-sync", formatterSnap)
	formatterCfg := formatters.NewSnapshotConfig(rootLogger.With("component", "formatter_config"), formatterSnap)

	formatterPool := formatters.NewPool(rootLogger.With("component", "formatter_pool"), formatterCfg, 0)

	pluginExecutor := formatters.NewPluginExecutor(rootLogger.With("component", "plugin_executor"), formatterPool.Sync, cfg.FormatterPluginDir, formatterSnap, formatterCfg)
	pluginExecutorSvc := services.NewManagedService("alert-formatter-sync", pluginExecutor)

	cfg.Config.ReadyFn = func() bool {
		return formatterSnap.Ready() && len(formatterCfg.Primaries()) > 0
	}
	formatterSvc := formatter.NewService(rootLogger.With("component", "service"), cfg.Config, formatterPool, formatterCfg)

	// Health only requires snapshot catch-up; consumption also requires a non-empty primary catalog.
	go services.ServeHealth(rootLogger.With("component", "health"), ":8080", formatterSnap.Ready)

	runner := services.New(rootLogger.With("component", "runner"))
	runner.Register(
		formatterSnapSvc,
		pluginExecutorSvc,
		formatterSvc,
	)
	runner.Run(ctx)
	rootLogger.Info("Shutting down alert-formatter")
}
