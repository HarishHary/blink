package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"ergo.services/application/mcp"
	"ergo.services/application/observer"
	"ergo.services/application/radar"
	"ergo.services/ergo"
	"ergo.services/ergo/gen"
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

type NodeOptions struct {
	Name            gen.Atom
	Env             string
	ShutdownTimeout time.Duration
	Applications    []gen.ApplicationBehavior
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
			observer.CreateApp(observer.Options{}),
			mcp.CreateApp(mcp.Options{Port: 9922}),
		)
		logLevel = gen.LogLevelDebug
	}
	applications = append(applications, opts.Applications...)

	n, err := ergo.StartNode(
		opts.Name,
		gen.NodeOptions{
			ShutdownTimeout: opts.ShutdownTimeout,
			Applications:    applications,
			Network: gen.NetworkOptions{
				Mode: gen.NetworkModeDisabled,
			},
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

func (h *NodeHost) Node() gen.Node {
	if h == nil {
		return nil
	}
	return h.node
}

func (h *NodeHost) Name() gen.Atom {
	if h == nil || h.node == nil {
		return ""
	}
	return h.node.Name()
}

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
		// Only the initiating caller may force shutdown.
		// Do not wait afterward because ctx has expired.
		h.node.StopForce()
		return fmt.Errorf("stop Ergo node: %w", ctx.Err())
	}
}

func (h *NodeHost) waitForStop(ctx context.Context) error {
	select {
	case <-h.stopped:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for Ergo node shutdown: %w", ctx.Err())
	}
}
