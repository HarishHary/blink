package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/cmd/rule_tuner/tuner"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/tuning_rules"
)

// runtimeShutdownTimeout bounds the Ergo node close after the Runner returns.
const runtimeShutdownTimeout = 45 * time.Second

// config is everything rule_tuner needs.
type config struct {
	services.Common
	tuner.Config
	plugin.EtcdClusterConfig
	ControllerNodeHost string `env:"CONTROLLER_NODE_HOST,optional"`
	PodName            string `env:"POD_NAME,optional"`
	PodIP              string `env:"POD_IP,optional"`
	TuningPluginDir    string `env:"TUNER_PLUGIN_DIR"`
}

// main runs the tuner service and exits if its runtime stops.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	cfg.Broker = brokers.NewKafkaBroker(cfg.Kafka)
	cfg.Config = cfg.Config.WithDefaults()
	rootLogger := logger.New("rule-tuner", cfg.Debug)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	controllerHost := cfg.ControllerNodeHost
	if controllerHost == "" {
		controllerHost = "controller"
	}
	nodeName := gen.Atom(fmt.Sprintf("rule-tuner-%s@%s", cfg.PodName, cfg.PodIP))
	registrar, err := plugin.NewEtcdRegistrar(cfg.EtcdClusterConfig, cfg.Env)
	if err != nil {
		rootLogger.FatalF("create etcd registrar: %v", err)
	}

	// Match admission limits to the service; leave the process budget at its CPU-based default.
	app := tuning_rules.NewApplication(plugin.ApplicationOptions{
		MaxBatchSize:       cfg.MaxBatchSize,
		MaxConcurrentCalls: cfg.MaxConcurrentCalls,
		Namespace:          "tuning",
		SupervisorOptions: plugin.SupervisorOptions{
			Directory: cfg.TuningPluginDir,
			SnapshotReader: snapshot.ReaderActorOptions{
				Endpoint:   gen.ProcessID{Name: snapshot.ControllerActorName("tuning"), Node: gen.Atom("controller@" + controllerHost)},
				ExecutorID: cfg.PodName,
			},
		},
	}, rootLogger)

	cluster := &plugin.ClusterOptions{Cookie: cfg.Cookie, Registrar: registrar, Flags: plugin.DefaultClusterFlags()}
	host, err := plugin.Start(plugin.NodeOptions{
		Name:            nodeName,
		Debug:           cfg.Debug,
		ShutdownTimeout: runtimeShutdownTimeout,
		Applications:    []gen.ApplicationBehavior{app},
		Cluster:         cluster,
		Observer:        plugin.EndpointOptions{Enabled: cfg.ObserverEnabled, Host: cfg.ObserverHost, Port: cfg.ObserverPort},
		MCP:             plugin.EndpointOptions{Enabled: cfg.MCPEnabled, Host: cfg.MCPHost, Port: cfg.MCPPort},
		Radar:           plugin.EndpointOptions{Enabled: cfg.RadarEnabled, Host: cfg.RadarHost, Port: cfg.RadarPort},
	})
	if err != nil {
		rootLogger.FatalF("rule-tuner: %v", err)
	}

	runnerStopped := make(chan error, 1)
	go func() {
		err := app.Wait(runCtx)
		if runCtx.Err() == nil {
			runnerStopped <- err
			cancelRun()
		}
	}()

	tunerSvc := tuner.NewService(rootLogger.With("component", "service"), cfg.Config, app)
	healthSvc := services.NewHealthService(":8080", tunerSvc.Ready, nil)
	runner := services.New(rootLogger.With("component", "runner"))
	runner.Register(tunerSvc, healthSvc)
	runner.Run(runCtx)

	var runnerErr error
	select {
	case err := <-runnerStopped:
		if err == nil {
			runnerErr = fmt.Errorf("rule-tuner runner stopped")
		} else {
			runnerErr = fmt.Errorf("rule-tuner runner stopped: %w", err)
		}
	default:
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), runtimeShutdownTimeout)
	if err := host.Close(shutdownCtx); err != nil {
		rootLogger.ErrorF("stop Ergo node: %v", err)
	}
	shutdownCancel()

	if runnerErr != nil {
		rootLogger.FatalF("rule-tuner runner: %v", runnerErr)
	}
	rootLogger.Info("Shutting down rule-tuner")
}
