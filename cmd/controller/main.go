package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ergo.services/application/observer"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime/controller"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/enrichments"
	"github.com/harishhary/blink/pkg/formatters"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/tuning_rules"
)

const runtimeShutdownTimeout = 45 * time.Second

type controllerConfig struct {
	services.Common
	ControllerDatabaseDSN  string `env:"CONTROLLER_DATABASE_DSN"`
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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg controllerConfig
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		slog.Error("load controller config", "error", err)
		os.Exit(1)
	}
	broker := brokers.NewKafkaBroker(cfg.Kafka)
	rootLogger := logger.New("controller", cfg.Env)
	host, err := plugin.Start(plugin.NodeOptions{
		Name:            "controller@localhost",
		ShutdownTimeout: runtimeShutdownTimeout,
		Applications: []gen.ApplicationBehavior{
			observer.CreateApp(observer.Options{}),
		},
	})
	if err != nil {
		rootLogger.FatalF("start controller node: %v", err)
	}

	node := host.Node()
	runner := services.New(rootLogger.With("component", "runner"))
	runner.Register(
		controller.NewService(node, "controller-rule", controller.ControllerApplicationOptions[*rules.RuleMetadata]{
			DatabaseDSN: cfg.ControllerDatabaseDSN,
			Namespace:   "rule",
			Topic:       cfg.ExecutorSnapshotTopic,
			Broker:      broker,
			SupervisorOptions: controller.ControllerSupervisorOptions[*rules.RuleMetadata]{
				ActorOptions: controller.ControllerActorOptions[*rules.RuleMetadata]{
					Directory: cfg.RulePluginDir,
					Loader:    rules.Loader{},
				},
			},
		}),
		controller.NewService(node, "controller-matcher", controller.ControllerApplicationOptions[*matchers.MatcherMetadata]{
			DatabaseDSN: cfg.ControllerDatabaseDSN,
			Namespace:   "matcher",
			Topic:       cfg.MatcherSnapshotTopic,
			Broker:      broker,
			SupervisorOptions: controller.ControllerSupervisorOptions[*matchers.MatcherMetadata]{
				ActorOptions: controller.ControllerActorOptions[*matchers.MatcherMetadata]{
					Directory: cfg.MatcherPluginDir,
					Loader:    matchers.Loader{},
				},
			},
		}),
		controller.NewService(node, "controller-tuning", controller.ControllerApplicationOptions[*tuning_rules.TuningRuleMetadata]{
			DatabaseDSN: cfg.ControllerDatabaseDSN,
			Namespace:   "tuning",
			Topic:       cfg.TunerSnapshotTopic,
			Broker:      broker,
			SupervisorOptions: controller.ControllerSupervisorOptions[*tuning_rules.TuningRuleMetadata]{
				ActorOptions: controller.ControllerActorOptions[*tuning_rules.TuningRuleMetadata]{
					Directory: cfg.TuningPluginDir,
					Loader:    tuning_rules.Loader{},
				},
			},
		}),
		controller.NewService(node, "controller-formatter", controller.ControllerApplicationOptions[*formatters.FormatterMetadata]{
			DatabaseDSN: cfg.ControllerDatabaseDSN,
			Namespace:   "formatter",
			Topic:       cfg.FormatterSnapshotTopic,
			Broker:      broker,
			SupervisorOptions: controller.ControllerSupervisorOptions[*formatters.FormatterMetadata]{
				ActorOptions: controller.ControllerActorOptions[*formatters.FormatterMetadata]{
					Directory: cfg.FormatterPluginDir,
					Loader:    formatters.Loader{},
				},
			},
		}),
		controller.NewService(node, "controller-enrichment", controller.ControllerApplicationOptions[*enrichments.EnrichmentMetadata]{
			DatabaseDSN: cfg.ControllerDatabaseDSN,
			Namespace:   "enrichment",
			Topic:       cfg.EnricherSnapshotTopic,
			Broker:      broker,
			SupervisorOptions: controller.ControllerSupervisorOptions[*enrichments.EnrichmentMetadata]{
				ActorOptions: controller.ControllerActorOptions[*enrichments.EnrichmentMetadata]{
					Directory: cfg.EnrichmentPluginDir,
					Loader:    enrichments.Loader{},
				},
			},
		}),
	)
	go services.ServeHealth(rootLogger.With("component", "health"), ":8080", nil)
	runner.Run(ctx)

	closeCtx, cancel := context.WithTimeout(context.Background(), runtimeShutdownTimeout)
	err = host.Close(closeCtx)
	cancel()
	if err != nil {
		rootLogger.FatalF("close controller node: %v", err)
	}
	rootLogger.Info("controller stopped")
}
