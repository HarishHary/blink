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

// PluginMetadata holds the common identity and rollout fields shared by all plugin types.
type PluginMetadata struct {
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

func (m PluginMetadata) Metadata() PluginMetadata { return m }
func (m *PluginMetadata) SetName(name string)     { m.Name = name }
func (m PluginMetadata) Checksum() string         { return "" }

type Syncable interface {
	Metadata() PluginMetadata
	Checksum() string
}

type RPC interface {
	Init(context.Context, *emptypb.Empty, ...grpc.CallOption) (*emptypb.Empty, error)
	Ping(context.Context, *emptypb.Empty, ...grpc.CallOption) (*emptypb.Empty, error)
	Shutdown(context.Context, *emptypb.Empty, ...grpc.CallOption) (*emptypb.Empty, error)
}

type Adapter[T Syncable] struct {
	Key         string
	Magic       string
	Plugin      goplugin.Plugin
	DoHandshake func(context.Context, any, Deployment) (T, RPC, error)
}

func (a *Adapter[T]) PluginKey() string           { return a.Key }
func (a *Adapter[T]) MagicValue() string          { return a.Magic }
func (a *Adapter[T]) GRPCPlugin() goplugin.Plugin { return a.Plugin }
func (a *Adapter[T]) Handshake(ctx context.Context, raw any, deployment Deployment) (T, RPC, error) {
	return a.DoHandshake(ctx, raw, deployment)
}

func Handshake[T any, C RPC](
	ctx context.Context,
	raw any,
	deployment Deployment,
	newPlugin func(string, C, string) T,
) (T, RPC, error) {
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
