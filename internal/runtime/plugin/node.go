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

// clusterAcceptorPort is the fallback when ClusterOptions.Port is zero; the controller's Helm
// cluster.port must match whatever this resolves to.
const clusterAcceptorPort = 11144

// NodeHost owns the single Ergo node for one Blink process; attached subtrees must never stop it.
type NodeHost struct {
	node     gen.Node
	stopOnce sync.Once
	stopped  chan struct{}
}

// NodeOptions configures a NodeHost.
type NodeOptions struct {
	Name            gen.Atom
	ShutdownTimeout time.Duration
	Debug           bool
	Applications    []gen.ApplicationBehavior
	Radar           EndpointOptions
	Observer        EndpointOptions
	MCP             EndpointOptions
	Cluster         *ClusterOptions
}

// EndpointOptions binds one of the node's HTTP endpoints: radar's metrics and readiness, Ergo's
// observer UI, or its MCP server. Each is off unless Enabled, independent of the others.
type EndpointOptions struct {
	Enabled bool
	Host    string
	Port    uint16
}

// Ports an empty EndpointOptions.Port binds, matching each app's own default.
const (
	defaultRadarPort    = 9090
	defaultObserverPort = 9911
	defaultMCPPort      = 9922
)

// defaultEndpointHost is what an empty EndpointOptions.Host binds: every app defaults to localhost,
// which nothing outside the process can reach, and an enabled endpoint is meant to be reached.
const defaultEndpointHost = "0.0.0.0"

// binding resolves an endpoint's host and port, substituting the defaults for empty values.
func (o EndpointOptions) binding(defaultPort uint16) (string, uint16) {
	host, port := o.Host, o.Port
	if host == "" {
		host = defaultEndpointHost
	}
	if port == 0 {
		port = defaultPort
	}
	return host, port
}

// ClusterOptions enables and configures Ergo cluster networking for a node. Port zero uses
// clusterAcceptorPort, pinned rather than port-scanned; nil Registrar is Ergo's dev-only one.
type ClusterOptions struct {
	Cookie    string
	Port      uint16
	Registrar gen.Registrar
	Flags     gen.NetworkFlags
}

// DefaultClusterFlags returns the safe base for customizing cluster network flags.
func DefaultClusterFlags() gen.NetworkFlags { return gen.DefaultNetworkFlags }

// EtcdClusterConfig configures the etcd-backed registrar every binary uses to join the Ergo
// cluster. Endpoints is comma-separated.
type EtcdClusterConfig struct {
	Endpoints   string `env:"ETCD_ENDPOINTS"`
	Cookie      string `env:"CLUSTER_COOKIE"`
	Port        uint16 `env:"CLUSTER_PORT,optional"`
	Username    string `env:"ETCD_USERNAME,optional"`
	Password    string `env:"ETCD_PASSWORD,optional"`
	ClusterName string `env:"ETCD_CLUSTER,optional"`
}

// NewEtcdRegistrar builds the production registrar, namespaced blink-<env> unless ClusterName
// overrides it, never etcd's shared "default".
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
		return nil, errors.New("node: name is required")
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = gen.DefaultShutdownTimeout
	}
	applications := []gen.ApplicationBehavior{}
	if opts.Radar.Enabled {
		host, port := opts.Radar.binding(defaultRadarPort)
		applications = append(applications, radar.CreateApp(radar.Options{Host: host, Port: port}))
	}
	if opts.Observer.Enabled {
		host, port := opts.Observer.binding(defaultObserverPort)
		applications = append(applications, observer.CreateApp(observer.Options{Host: host, Port: port}))
	}
	if opts.MCP.Enabled {
		host, port := opts.MCP.binding(defaultMCPPort)
		applications = append(applications, mcp.CreateApp(mcp.Options{Host: host, Port: port}))
	}
	applications = append(applications, opts.Applications...)

	logLevel := gen.LogLevelInfo
	if opts.Debug {
		logLevel = gen.LogLevelDebug
	}

	network := gen.NetworkOptions{Mode: gen.NetworkModeDisabled}
	if opts.Cluster != nil {
		port := opts.Cluster.Port
		if port == 0 {
			port = clusterAcceptorPort
		}
		network = gen.NetworkOptions{
			Mode:      gen.NetworkModeEnabled,
			Cookie:    opts.Cluster.Cookie,
			Registrar: opts.Cluster.Registrar,
			Flags:     opts.Cluster.Flags,
			Acceptors: []gen.AcceptorOptions{{Host: "0.0.0.0", Port: port, PortRange: 1}},
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

	// Another caller already initiated shutdown; this one only waits.
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
		// ShutdownTimeout owns escalation: StopForce cannot replace a Stop already in progress.
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
