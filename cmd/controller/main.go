package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/controller"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/enrichments"
	"github.com/harishhary/blink/pkg/formatters"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/tuning_rules"
)

type controllerConfig struct {
	services.Common
	ExecutorSnapshotTopic  string `env:"KAFKA_TOPIC_EXECUTOR_SNAPSHOT"`
	MatcherSnapshotTopic   string `env:"KAFKA_TOPIC_MATCHER_SNAPSHOT"`
	TunerSnapshotTopic     string `env:"KAFKA_TOPIC_TUNER_SNAPSHOT"`
	FormatterSnapshotTopic string `env:"KAFKA_TOPIC_FORMATTER_SNAPSHOT"`
	EnricherSnapshotTopic  string `env:"KAFKA_TOPIC_ENRICHER_SNAPSHOT"`
	RulePluginDir          string `env:"RULE_PLUGIN_DIR"`
	MatcherPluginDir       string `env:"MATCHER_PLUGIN_DIR"`
	TuningPluginDir        string `env:"TUNER_PLUGIN_DIR"`
	FormatterPluginDir     string `env:"FORMATTER_PLUGIN_DIR"`
	EnrichmentPluginDir    string `env:"ENRICHER_PLUGIN_DIR"`
}

// main runs the control plane: one PluginController per plugin type. See docs/services/controller.md.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg controllerConfig
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	b := brokers.NewKafkaBroker(cfg.Kafka)
	rootLogger := logger.New("controller", cfg.Env)

	// Shared control-plane store; NewNop is the no-op default (swap for SQLite/Postgres).
	db := backends.NewNop()

	runner := services.New()

	addController(rootLogger, "rule", cfg.RulePluginDir, runner, db, rules.Loader{}, b.NewWriter(cfg.ExecutorSnapshotTopic))
	addController(rootLogger, "matcher", cfg.MatcherPluginDir, runner, db, matchers.Loader{}, b.NewWriter(cfg.MatcherSnapshotTopic))
	addController(rootLogger, "tuning", cfg.TuningPluginDir, runner, db, tuning_rules.Loader{}, b.NewWriter(cfg.TunerSnapshotTopic))
	addController(rootLogger, "formatter", cfg.FormatterPluginDir, runner, db, formatters.Loader{}, b.NewWriter(cfg.FormatterSnapshotTopic))
	addController(rootLogger, "enrichment", cfg.EnrichmentPluginDir, runner, db, enrichments.Loader{}, b.NewWriter(cfg.EnricherSnapshotTopic))

	go services.ServeHealth(":8080", nil)

	runner.Run(ctx)
	log.Println("Shutting down controller")
}

// addController registers a LocalReader + PluginController for one plugin type. See docs/services/controller.md.
func addController[T plugin.Syncable](
	rootLogger *logger.Logger,
	name string,
	dir string,
	runner *services.Runner,
	db backends.Database,
	loader config.Loader[T],
	writer brokers.Writer,
) {
	reader := controller.NewLocalReader(rootLogger.With("plugin_type", name, "component", "local_reader"), dir, loader)
	readerSvc := services.NewManagedService(name+"config-reader", reader)

	ctrl := controller.NewPluginController(rootLogger.With("plugin_type", name, "component", "plugin_controller"), db, reader, writer)
	ctrlSvc := services.NewManagedService(name+"-controller", ctrl)

	runner.Register(
		readerSvc,
		ctrlSvc,
	)
}
