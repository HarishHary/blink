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
	ruleOpts snapshot.SupervisorOptions[*rules.RuleMetadata]
}

// Load adds the rule snapshot supervisor to the matcher application's spec.
func (a *application) Load(...any) (gen.ApplicationSpec, error) {
	spec, err := a.Application.Load()
	if err != nil {
		return gen.ApplicationSpec{}, err
	}
	spec.Group = append(spec.Group, gen.ApplicationMemberSpec{
		Factory: func() gen.ProcessBehavior {
			return snapshot.NewSupervisor(a.ruleOpts)
		},
	})
	spec.Map["rule_snapshot"] = a.ruleOpts.ReaderActorOptions.Name
	return spec, nil
}

// main starts the Ergo node, the matcher runtime, and the Runner, and runs them for the process lifetime.
// If the runtime stops, main exits and the pod restarts it.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if err := services.LoadFromEnvironment(&cfg); err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	cfg.Broker = brokers.NewKafkaBroker(cfg.Kafka)
	rootLogger := logger.New("event-matcher", cfg.Env)

	runCtx, cancelRun := context.WithCancel(ctx)

	controllerHost := cfg.ControllerNodeHost
	if controllerHost == "" {
		controllerHost = "controller"
	}
	nodeName := gen.Atom(fmt.Sprintf("event-matcher-%s@%s", cfg.PodName, cfg.PodIP))
	registrar, err := plugin.NewEtcdRegistrar(cfg.EtcdClusterConfig, cfg.Env)
	if err != nil {
		cancelRun()
		rootLogger.FatalF("create etcd registrar: %v", err)
	}

	// snapshotReaderFor subscribes to the given namespace's controller actor over the cluster.
	snapshotReaderFor := func(name gen.Atom, namespace string) snapshot.ReaderActorOptions {
		return snapshot.ReaderActorOptions{
			Name:       name,
			Endpoint:   gen.ProcessID{Name: gen.Atom("controller-" + namespace + "-actor"), Node: gen.Atom("controller@" + controllerHost)},
			ExecutorID: cfg.PodName,
			Role:       namespace,
		}
	}

	app := &application{
		// Admission budgets come from the reader's batch size and the service's concurrency limit.
		// ManagerOptions.ProcessBudget defaults to available CPUs when left unset.
		Application: matchers.NewApplication(plugin.Options{MaxBatchSize: cfg.BatchSize, MaxConcurrentCalls: cfg.Concurrency,
			SupervisorOptions: plugin.SupervisorOptions{
				Name:           gen.Atom("event-matcher-runtime"),
				Directory:      cfg.MatcherPluginDir,
				SnapshotReader: snapshotReaderFor("matcher-snapshot-reader", "matcher"),
			}}, rootLogger),
		ruleOpts: snapshot.SupervisorOptions[*rules.RuleMetadata]{
			ReaderActorOptions: snapshotReaderFor("rule-snapshot-reader", "rule"),
			Loader:             rules.Loader{}, ProjectionMode: snapshot.ProjectionCommitDirect,
		},
	}

	cluster := &plugin.ClusterOptions{Cookie: cfg.Cookie, Port: cfg.Port, Registrar: registrar, Flags: plugin.DefaultClusterFlags()}
	host, err := plugin.Start(plugin.NodeOptions{
		Name:            nodeName,
		Env:             cfg.Env,
		ShutdownTimeout: runtimeShutdownTimeout,
		Applications:    []gen.ApplicationBehavior{app},
		Cluster:         cluster,
	})
	if err != nil {
		cancelRun()
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

	matcherSvc := NewService(rootLogger.With("component", "service"), cfg.Config, app, snapshot.NewProjectionClient[*rules.RuleMetadata](host.Node(), gen.Atom("rule-snapshot-reader")))
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
	cancelRun()

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
