package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ergo.services/application/mcp"
	"ergo.services/application/observer"
	"ergo.services/application/radar"
	"ergo.services/ergo"
	"ergo.services/ergo/gen"
	"ergo.services/registrar/etcd"
)

// Host owns the single Ergo node for one Blink process.
//
// Actor runtimes attach supervisor subtrees to this node. They must never stop
// the node themselves; only Host owns the node-wide shutdown boundary.
type NodeHost struct {
	node gen.Node

	stopOnce sync.Once
	stopped  chan struct{}
}

// NodeOptions configures a NodeHost.
type NodeOptions struct {
	Name            gen.Atom
	Env             string
	ShutdownTimeout time.Duration
	Applications    []gen.ApplicationBehavior
	// Cluster enables Ergo cluster networking (remote actor Send/Call between nodes). Nil keeps
	// today's default: networking disabled, a single-process node.
	Cluster *ClusterOptions
}

// ClusterOptions enables and configures Ergo cluster networking for a node.
type ClusterOptions struct {
	// Cookie authenticates connections between nodes; every node in the cluster must share it.
	Cookie string
	// Registrar resolves peer nodes for discovery. Nil falls back to Ergo's embedded, single-
	// process, dev-only registrar - never use nil in production.
	Registrar gen.Registrar
	// Flags controls cluster network capabilities (remote spawn, important delivery, etc.).
	// Build it from DefaultClusterFlags(), never a bare gen.NetworkFlags{Enable: true} literal -
	// leaving Flags zero here is fine (Ergo substitutes its own default), but once Enable is set
	// every flag not explicitly listed reverts to false, silently including EnableImportantDelivery.
	Flags gen.NetworkFlags
}

// DefaultClusterFlags returns gen.DefaultNetworkFlags, the safe starting point for any caller that
// needs to customize cluster network flags: override only what you mean to change on the result,
// never build a gen.NetworkFlags{Enable: true, ...} literal from scratch.
func DefaultClusterFlags() gen.NetworkFlags { return gen.DefaultNetworkFlags }

// EtcdClusterConfig configures the etcd-backed registrar every binary uses to join the Ergo
// cluster, following the services.Common env-tag convention. Endpoints is a comma-separated list,
// matching brokers.KafkaConfig.Brokers' convention.
type EtcdClusterConfig struct {
	Endpoints   string `env:"ETCD_ENDPOINTS"`
	Cookie      string `env:"CLUSTER_COOKIE"`
	Username    string `env:"ETCD_USERNAME,optional"`
	Password    string `env:"ETCD_PASSWORD,optional"`
	ClusterName string `env:"ETCD_CLUSTER,optional"`
}

// NewEtcdRegistrar builds the production registrar, namespacing it to blink-<env> unless
// ClusterName overrides it. An empty Cluster falls back to etcd's shared "default" namespace
// across every environment - NewEtcdRegistrar always avoids that footgun.
func NewEtcdRegistrar(cfg EtcdClusterConfig, env string) (gen.Registrar, error) {
	cluster := cfg.ClusterName
	if cluster == "" {
		cluster = "blink-" + env
	}
	return etcd.Create(etcd.Options{
		Cluster:   cluster,
		Endpoints: strings.Split(cfg.Endpoints, ","),
		Username:  cfg.Username,
		Password:  cfg.Password,
	})
}

// Start creates a node whose lifecycle is owned by the returned host.
func Start(opts NodeOptions) (*NodeHost, error) {
	if opts.Name == "" {
		return nil, errors.New("actornode: name is required")
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = gen.DefaultShutdownTimeout
	}
	applications := []gen.ApplicationBehavior{radar.CreateApp(radar.Options{})}
	logLevel := gen.LogLevelInfo
	if opts.Env == "dev" {
		applications = append(applications,
			observer.CreateApp(observer.Options{Port: 9911}),
			mcp.CreateApp(mcp.Options{Port: 9922}),
		)
		logLevel = gen.LogLevelDebug
	}
	applications = append(applications, opts.Applications...)

	network := gen.NetworkOptions{Mode: gen.NetworkModeDisabled}
	if opts.Cluster != nil {
		network = gen.NetworkOptions{
			Mode:      gen.NetworkModeEnabled,
			Cookie:    opts.Cluster.Cookie,
			Registrar: opts.Cluster.Registrar,
			Flags:     opts.Cluster.Flags,
			Acceptors: []gen.AcceptorOptions{{Host: "0.0.0.0"}},
		}
	}

	n, err := ergo.StartNode(
		opts.Name,
		gen.NodeOptions{
			ShutdownTimeout: opts.ShutdownTimeout,
			Applications:    applications,
			Network:         network,
			Log: gen.LogOptions{
				Level: logLevel,
				DefaultLogger: gen.DefaultLoggerOptions{
					TimeFormat:      time.RFC3339,
					IncludeBehavior: true,
					IncludeName:     true,
					IncludeFields:   true,
					DisableBanner:   true,
				},
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("start Ergo node %q: %w", opts.Name, err)
	}
	n.SetCTRLC(false)
	return &NodeHost{
		node:    n,
		stopped: make(chan struct{}),
	}, nil
}

// Node returns the host's Ergo node.
func (h *NodeHost) Node() gen.Node {
	if h == nil {
		return nil
	}
	return h.node
}

// Name returns the host node's name.
func (h *NodeHost) Name() gen.Atom {
	if h == nil || h.node == nil {
		return ""
	}
	return h.node.Name()
}

// Close stops the host node or waits for an existing shutdown.
func (h *NodeHost) Close(ctx context.Context) error {
	if h == nil || h.node == nil {
		return nil
	}

	initiator := false
	h.stopOnce.Do(func() {
		initiator = true
		go func() {
			h.node.Stop()
			close(h.stopped)
		}()
	})

	// Another caller already initiated shutdown. This caller is only a waiter.
	if !initiator {
		return h.waitForStop(ctx)
	}

	select {
	case <-h.stopped:
		return nil

	case <-ctx.Done():
		// Graceful shutdown may have completed concurrently with cancellation.
		select {
		case <-h.stopped:
			return nil
		default:
		}
		// The node's configured ShutdownTimeout owns escalation. StopForce cannot
		// replace a graceful Stop that is already in progress.
		return fmt.Errorf("stop Ergo node: %w", ctx.Err())
	}
}

// waitForStop waits for the host node to stop.
func (h *NodeHost) waitForStop(ctx context.Context) error {
	select {
	case <-h.stopped:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for Ergo node shutdown: %w", ctx.Err())
	}
}
