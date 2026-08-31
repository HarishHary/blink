package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slog"
	"syscall"
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
)

// runtimeShutdownTimeout bounds the Ergo node close after the Runner returns.
const runtimeShutdownTimeout = 45 * time.Second

// config is everything event_matcher needs. See docs/services/event_matcher.md.
type config struct {
	services.Common
	Config
	plugin.EtcdClusterConfig
	// ControllerNodeHost names the controller node this executor subscribes to over the
	// cluster. Must match cmd/controller's CONTROLLER_NODE_HOST.
	ControllerNodeHost string `env:"CONTROLLER_NODE_HOST,optional"`
	PodName            string `env:"POD_NAME,optional"`
	PodIP              string `env:"POD_IP,optional"`
	MatcherPluginDir   string `env:"MATCHER_PLUGIN_DIR"`
}

// application is the matcher plugin runtime plus the rule snapshot supervisor, so both start and stop with the node.
type application struct {
	*matchers.Application
	ruleOpts snapshot.SupervisorOptions
}

// Load adds the rule snapshot supervisor to the matcher application's spec.
func (a *application) Load(...any) (gen.ApplicationSpec, error) {
	spec, err := a.Application.Load()
	if err != nil {
		return gen.ApplicationSpec{}, err
	}
	spec.Group = append(spec.Group, gen.ApplicationMemberSpec{
		Factory: func() gen.ProcessBehavior {
			return snapshot.NewSupervisor(a.ruleOpts, rules.Loader{})
		},
	})
	spec.Map["rule_snapshot"] = snapshot.SupervisorName(a.ruleOpts.Namespace)
	return spec, nil
}

// main starts the Ergo node, the matcher runtime, and the Runner, and runs them for the process lifetime. If the runtime stops, main exits and the pod restarts it.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		slog.Fatalf("load config: %v", err)
	}
	cfg.Broker = brokers.NewKafkaBroker(cfg.Kafka)
	rootLogger := logger.New("event-matcher", cfg.Debug)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	controllerHost := cfg.ControllerNodeHost
	if controllerHost == "" {
		controllerHost = "controller"
	}
	nodeName := gen.Atom(fmt.Sprintf("event-matcher-%s@%s", cfg.PodName, cfg.PodIP))
	registrar, err := plugin.NewEtcdRegistrar(cfg.EtcdClusterConfig, cfg.Env)
	if err != nil {
		rootLogger.FatalF("create etcd registrar: %v", err)
	}

	// snapshotReaderFor subscribes to the given namespace's controller actor over the cluster.
	snapshotReaderFor := func(namespace string) snapshot.ReaderActorOptions {
		return snapshot.ReaderActorOptions{
			Endpoint:   gen.ProcessID{Name: snapshot.ControllerActorName(namespace), Node: gen.Atom("controller@" + controllerHost)},
			ExecutorID: cfg.PodName,
		}
	}

	app := &application{
		// Admission budgets come from the reader's batch size and the service's concurrency limit.
		// ManagerOptions.ProcessBudget defaults to available CPUs when left unset.
		Application: matchers.NewApplication(plugin.ApplicationOptions{MaxBatchSize: cfg.BatchSize, MaxConcurrentCalls: cfg.Concurrency, Namespace: "matcher",
			SupervisorOptions: plugin.SupervisorOptions{
				Directory:      cfg.MatcherPluginDir,
				SnapshotReader: snapshotReaderFor("matcher"),
			}}, rootLogger),
		ruleOpts: snapshot.SupervisorOptions{
			Namespace:          "rule",
			ReaderActorOptions: snapshotReaderFor("rule"),
			ProjectionMode:     snapshot.ProjectionCommitDirect,
		},
	}

	cluster := &plugin.ClusterOptions{Cookie: cfg.Cookie, Port: cfg.Port, Registrar: registrar, Flags: plugin.DefaultClusterFlags()}
	host, err := plugin.Start(plugin.NodeOptions{
		Name:            nodeName,
		Debug:           cfg.Debug,
		Observer:        plugin.EndpointOptions{Enabled: cfg.ObserverEnabled, Host: cfg.ObserverHost, Port: cfg.ObserverPort},
		MCP:             plugin.EndpointOptions{Enabled: cfg.MCPEnabled, Host: cfg.MCPHost, Port: cfg.MCPPort},
		ShutdownTimeout: runtimeShutdownTimeout,
		Applications:    []gen.ApplicationBehavior{app},
		Cluster:         cluster,
		Radar:           plugin.EndpointOptions{Enabled: cfg.RadarEnabled, Host: cfg.RadarHost, Port: cfg.RadarPort},
	})
	if err != nil {
		rootLogger.FatalF("event-matcher: %v", err)
	}

	runnerStopped := make(chan error, 1)
	go func() {
		err := app.Wait(runCtx)
		if runCtx.Err() == nil {
			runnerStopped <- err
			cancelRun()
		}
	}()

	matcherSvc := NewService(rootLogger.With("component", "service"), cfg.Config, app, snapshot.NewProjectionClient[*rules.RuleMetadata](host.Node(), "rule"))
	healthSvc := services.NewHealthService(":8080", matcherSvc.Ready, nil)
	runner := services.New(rootLogger.With("component", "runner"))
	runner.Register(matcherSvc, healthSvc)
	runner.Run(runCtx)

	var runnerErr error
	select {
	case err := <-runnerStopped:
		if err == nil {
			runnerErr = fmt.Errorf("event-matcher runner stopped")
		} else {
			runnerErr = fmt.Errorf("event-matcher runner stopped: %w", err)
		}
	default:
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), runtimeShutdownTimeout)
	if err := host.Close(shutdownCtx); err != nil {
		rootLogger.ErrorF("stop Ergo node: %v", err)
	}
	shutdownCancel()

	if runnerErr != nil {
		rootLogger.FatalF("event-matcher runner: %v", runnerErr)
	}
	rootLogger.Info("Shutting down event-matcher")
}
