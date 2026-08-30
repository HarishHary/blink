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
	Env             string
	ShutdownTimeout time.Duration
	Applications    []gen.ApplicationBehavior
	Observer        ObserverOptions
	// Cluster nil leaves the node single-process; Radar nil keeps radar's own localhost:9090.
	Cluster *ClusterOptions
	Radar   *RadarOptions
}

// ObserverOptions binds Ergo's observer UI. Enabled is separate because Env == "dev" enables it anyway
type ObserverOptions struct {
	Enabled bool
	Host    string
	Port    uint16
}

// defaultObserverPort is what an empty ObserverOptions.Port binds.
const defaultObserverPort = 9911

// RadarOptions binds radar's health and Prometheus endpoints. Radar defaults to localhost, which no
// scraper outside the process can reach, so an empty Host binds all interfaces; an empty Port keeps 9090.
type RadarOptions struct {
	Host string
	Port uint16
}

// defaultRadarHost is what an empty RadarOptions.Host binds: passing the options at all means the
// endpoint is meant to be reachable from outside the pod.
const defaultRadarHost = "0.0.0.0"

// ClusterOptions enables and configures Ergo cluster networking for a node. Port zero uses
// clusterAcceptorPort, pinned rather than port-scanned; nil Registrar is Ergo's dev-only one.
type ClusterOptions struct {
	Cookie    string
	Port      uint16
	Registrar gen.Registrar
	// Flags must come from DefaultClusterFlags(): setting Enable in a bare literal zeroes the rest.
	Flags gen.NetworkFlags
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
	radarOptions := radar.Options{}
	if opts.Radar != nil {
		radarOptions.Host, radarOptions.Port = opts.Radar.Host, opts.Radar.Port
		if radarOptions.Host == "" {
			radarOptions.Host = defaultRadarHost
		}
	}
	applications := []gen.ApplicationBehavior{radar.CreateApp(radarOptions)}
	logLevel := gen.LogLevelInfo
	if opts.Env == "dev" {
		applications = append(applications, mcp.CreateApp(mcp.Options{Port: 9922}))
		logLevel = gen.LogLevelDebug
	}
	if opts.Env == "dev" || opts.Observer.Enabled {
		observerOptions := observer.Options{Host: opts.Observer.Host, Port: opts.Observer.Port}
		if observerOptions.Port == 0 {
			observerOptions.Port = defaultObserverPort
		}
		applications = append(applications, observer.CreateApp(observerOptions))
	}
	applications = append(applications, opts.Applications...)

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
