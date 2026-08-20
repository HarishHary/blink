package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/runtime"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Spec holds the common identity and rollout fields shared by all plugin types.
type Spec struct {
	Id          string              `yaml:"id"`
	Name        string              `yaml:"-"`
	DisplayName string              `yaml:"display_name"`
	Description string              `yaml:"description"`
	Enabled     bool                `yaml:"enabled"`
	Version     string              `yaml:"version"`
	RolloutMode runtime.RolloutMode `yaml:"mode"`
	RolloutPct  float64             `yaml:"rollout_pct"`
	MinProcs    int                 `yaml:"min_procs"`
	MaxProcs    int                 `yaml:"max_procs"`
}

func (s Spec) Spec() Spec { return s }

// Metadata returns the plugin metadata.
func (s Spec) Metadata() Spec { return s }

// SetName sets the plugin name.
func (s *Spec) SetName(name string) { s.Name = name }

// Checksum returns the plugin checksum.
func (s Spec) Checksum() string { return "" }

// Syncable defines the metadata contract for synchronizable plugins.
type Syncable interface {
	Metadata() Spec
	Checksum() string
}

// RPC defines the lifecycle RPCs for a plugin.
type RPC interface {
	Init(context.Context, *emptypb.Empty, ...grpc.CallOption) (*emptypb.Empty, error)
	Ping(context.Context, *emptypb.Empty, ...grpc.CallOption) (*emptypb.Empty, error)
	Shutdown(context.Context, *emptypb.Empty, ...grpc.CallOption) (*emptypb.Empty, error)
}

// Adapter describes the go-plugin adapter for a plugin type.
type Adapter[T Syncable] struct {
	Key         string
	Magic       string
	Plugin      goplugin.Plugin
	DoHandshake func(context.Context, any, Deployment) (T, RPC, error)
}

// PluginKey returns the adapter plugin key.
func (a *Adapter[T]) PluginKey() string { return a.Key }

// MagicValue returns the adapter magic value.
func (a *Adapter[T]) MagicValue() string { return a.Magic }

// GRPCPlugin returns the adapter gRPC plugin.
func (a *Adapter[T]) GRPCPlugin() goplugin.Plugin { return a.Plugin }

// Handshake delegates to the adapter handshake function.
func (a *Adapter[T]) Handshake(ctx context.Context, raw any, deployment Deployment) (T, RPC, error) {
	return a.DoHandshake(ctx, raw, deployment)
}

// Handshake initializes a plugin RPC and returns its typed plugin.
func Handshake[T any, C RPC](ctx context.Context, raw any, deployment Deployment, newPlugin func(string, C, string) T) (T, RPC, error) {
	var zero T
	rpc, ok := raw.(C)
	if !ok {
		return zero, nil, fmt.Errorf("dispense: unexpected type %T", raw)
	}

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := rpc.Init(initCtx, &emptypb.Empty{}); err != nil {
		return zero, nil, fmt.Errorf("init: %w", err)
	}
	return newPlugin(helpers.BinaryBaseName(deployment.Path), rpc, deployment.Hash), rpc, nil
}
