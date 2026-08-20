package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/cmd/event_matcher/matcher"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/rules"
)

const runtimeShutdownTimeout = 45 * time.Second

// config is everything event_matcher needs. See docs/services/event_matcher.md.
type config struct {
	services.Common
	matcher.Config
	MatcherSnapshotTopic  string `env:"KAFKA_TOPIC_MATCHER_SNAPSHOT"`
	ExecutorSnapshotTopic string `env:"KAFKA_TOPIC_EXECUTOR_SNAPSHOT"`
	MatcherPluginDir      string `env:"MATCHER_PLUGIN_DIR"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	cfg.Config.Broker = brokers.NewKafkaBroker(cfg.Kafka)
	rootLogger := logger.New("event-matcher", cfg.Env)
	broker := cfg.Config.Broker
	opts := matcher.Options{
		ApplicationOpts: plugin.Options{SupervisorOptions: plugin.SupervisorOptions{
			Name:      gen.Atom("event-matcher-runtime"),
			Directory: cfg.MatcherPluginDir,
			SnapshotReader: snapshot.ReaderActorOptions{
				Name:          gen.Atom("event-matcher-runtime"),
				Logger:        rootLogger.With("component", "matcher_snapshot"),
				ReaderFactory: func() brokers.Reader { return broker.NewBroadcastReader(cfg.MatcherSnapshotTopic) },
			},
		}},
		RuleSnapshotOpts: snapshot.SupervisorOptions[*rules.RuleMetadata]{
			ReaderActorOptions: snapshot.ReaderActorOptions{
				Name:          gen.Atom("event-matcher-rule-snapshot"),
				Logger:        rootLogger.With("component", "rule_snapshot"),
				ReaderFactory: func() brokers.Reader { return broker.NewBroadcastReader(cfg.ExecutorSnapshotTopic) },
			},
			Loader: rules.Loader{}, ProjectionMode: snapshot.ProjectionCommitDirect,
		},
	}
	host, err := plugin.Start(plugin.NodeOptions{
		Name:            "event-matcher@localhost",
		Env:             cfg.Env,
		ShutdownTimeout: runtimeShutdownTimeout,
	})
	if err != nil {
		rootLogger.FatalF("start Ergo node: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), runtimeShutdownTimeout)
		defer shutdownCancel()
		if err := host.Close(shutdownCtx); err != nil {
			rootLogger.ErrorF("stop Ergo node: %v", err)
		}
	}()

	matcherSvc := matcher.NewService(host.Node(), rootLogger.With("component", "service"), cfg.Config, opts)
	healthSvc := services.NewHealthService(":8080", matcherSvc.Ready)
	runner := services.New(rootLogger.With("component", "runner"))
	runner.Register(matcherSvc, healthSvc)
	runner.Run(ctx)
	rootLogger.Info("Shutting down event-matcher")
}
