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

type ControllerConfig struct {
	services.Common
	RuleSnapshotTopic       string `env:"KAFKA_TOPIC_RULE_SNAPSHOT"`
	MatcherSnapshotTopic    string `env:"KAFKA_TOPIC_MATCHER_SNAPSHOT"`
	TuningSnapshotTopic     string `env:"KAFKA_TOPIC_TUNING_SNAPSHOT"`
	FormatterSnapshotTopic  string `env:"KAFKA_TOPIC_FORMATTER_SNAPSHOT"`
	EnrichmentSnapshotTopic string `env:"KAFKA_TOPIC_ENRICHMENT_SNAPSHOT"`
	RulePluginDir           string `env:"RULE_PLUGIN_DIR"`
	MatcherPluginDir        string `env:"MATCHER_PLUGIN_DIR"`
	TuningPluginDir         string `env:"TUNER_PLUGIN_DIR"`
	FormatterPluginDir      string `env:"FORMATTER_PLUGIN_DIR"`
	EnrichmentPluginDir     string `env:"ENRICHER_PLUGIN_DIR"`
}

// controller is the unified control plane: one PluginController per plugin type, each
// reconciling its own plugin dir + YAML and publishing its effective Snapshot to its own
// topic.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg ControllerConfig
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	b := brokers.NewKafkaBroker(cfg.Kafka)

	// Control-plane persistence shared by every type. NewNop is the no-op default;
	// swap for a real store (SQLite/Postgres) when persistence is needed.
	db := backends.NewNop()

	runner := services.New()

	addController("rule", cfg.RulePluginDir, runner, db, rules.Loader{}, b.NewWriter(cfg.RuleSnapshotTopic))
	addController("matcher", cfg.MatcherPluginDir, runner, db, matchers.Loader{}, b.NewWriter(cfg.MatcherSnapshotTopic))
	addController("tuning", cfg.TuningPluginDir, runner, db, tuning_rules.Loader{}, b.NewWriter(cfg.TuningSnapshotTopic))
	addController("formatter", cfg.FormatterPluginDir, runner, db, formatters.Loader{}, b.NewWriter(cfg.FormatterSnapshotTopic))
	addController("enrichment", cfg.EnrichmentPluginDir, runner, db, enrichments.Loader{}, b.NewWriter(cfg.EnrichmentSnapshotTopic))

	go services.ServeHealth(":8080", nil)

	runner.Run(ctx)
	log.Println("Shutting down controller")
}

// addController wires a LocalReader + PluginController for one plugin type and registers both as
// services: the reader watches dir and exposes the parsed catalog; the controller reconciles it
// against Postgres and publishes. loader parses one YAML sidecar into T.
func addController[T plugin.Syncable](
	name string,
	dir string,
	runner *services.Runner,
	db backends.Database,
	loader config.Loader[T],
	writer brokers.Writer,
) {
	readerName := name + "-config"

	reader := controller.NewLocalReader(logger.New(readerName, "dev"), readerName, dir, loader)
	readerSvc := services.NewManagedService(readerName+"-reader", reader)

	ctrl := controller.NewPluginController(logger.New(name+"-controller", "dev"), db, reader, writer)
	ctrlSvc := services.NewManagedService(name+"-controller-sync", ctrl)

	runner.Register(
		readerSvc,
		ctrlSvc,
	)
}
