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

type PluginRPC interface {
	Init(context.Context, *emptypb.Empty, ...grpc.CallOption) (*emptypb.Empty, error)
	Ping(context.Context, *emptypb.Empty, ...grpc.CallOption) (*emptypb.Empty, error)
	Shutdown(context.Context, *emptypb.Empty, ...grpc.CallOption) (*emptypb.Empty, error)
}

type BinaryState struct {
	Id         string
	Name       string
	Enabled    bool
	Mode       runtime.RolloutMode
	RolloutPct float64
	MaxProcs   int
}

type DesiredConfig interface {
	DesiredBinaryState(name string) (BinaryState, bool)
}

type PluginAdapter[T Syncable] struct {
	Key         string
	Magic       string
	Plugin      goplugin.Plugin
	Config      DesiredConfig
	DoHandshake func(context.Context, any, string, string) (T, PluginRPC, error)
}

func (a *PluginAdapter[T]) PluginKey() string           { return a.Key }
func (a *PluginAdapter[T]) MagicValue() string          { return a.Magic }
func (a *PluginAdapter[T]) GRPCPlugin() goplugin.Plugin { return a.Plugin }
func (a *PluginAdapter[T]) Handshake(ctx context.Context, raw any, path, hash string) (T, PluginRPC, error) {
	return a.DoHandshake(ctx, raw, path, hash)
}

func Handshake[T any, C PluginRPC](
	ctx context.Context,
	raw any,
	binPath string,
	hash string,
	newPlugin func(string, C, string) T,
) (T, PluginRPC, error) {
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
	return newPlugin(helpers.BinaryBaseName(binPath), rpc, hash), rpc, nil
}
