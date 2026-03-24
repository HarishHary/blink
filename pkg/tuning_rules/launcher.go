package tuning_rules

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/tuning_rules/config"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

type TuningRuleAdapter struct {
	Watcher *config.Watcher
}

func (l *TuningRuleAdapter) PluginKey() string         { return "tuning_rule" }
func (l *TuningRuleAdapter) MagicValue() string        { return "tuning_rule_v1" }
func (l *TuningRuleAdapter) GRPCPlugin() goplugin.Plugin { return &tuningPlugin{} }

// Handshake connects to the tuning rule subprocess, calls Init, and returns a
// ready rpcTuningRule. Identity comes from the YAML sidecar, not from a GetMetadata RPC.
func (l *TuningRuleAdapter) Handshake(ctx context.Context, raw interface{}, binPath string, hash string) (TuningRule, plugin.PluginLifecycle, string, string, error) {
	rpc, ok := raw.(rpc_tuning_rules.TuningRuleClient)
	if !ok {
		return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
	}

	fileName := helpers.BinaryBaseName(binPath)

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := rpc.Init(initCtx, &rpc_tuning_rules.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("init: %w", err)
	}

	tr := newRpcTuningRule(fileName, rpc, l.Watcher, hash)
	cfg, ok := l.Watcher.Current().ByFileName(fileName)
	id, name := fileName, fileName
	if ok {
		id = cfg.Id
		name = cfg.Name
	}
	return tr, &tuningLifecycle{rpc: rpc}, id, name, nil
}

// IsReady reports whether this binary's YAML sidecar exists in the current registry.
func (l *TuningRuleAdapter) IsReady(binPath string) bool {
	_, ok := l.Watcher.Current().ByFileName(helpers.BinaryBaseName(binPath))
	return ok
}

// IsShadow reports whether this binary's YAML declares it as a shadow or canary version.
func (l *TuningRuleAdapter) IsShadow(binPath string) bool {
	cfg, ok := l.Watcher.Current().ByFileName(helpers.BinaryBaseName(binPath))
	if !ok {
		return false
	}
	m := cfg.RolloutMode
	return m == internal.RolloutModeCanary || m == internal.RolloutModeShadow
}

// IsEnabled reports whether the tuning rule's YAML sidecar still exists and is enabled.
func (l *TuningRuleAdapter) IsEnabled(h *plugin.PluginHandle) bool {
	cfg, ok := l.Watcher.Current().ByFileName(helpers.BinaryBaseName(h.BinPath))
	return ok && cfg.Enabled
}

func (l *TuningRuleAdapter) Workers(binPath string) int {
	cfg, ok := l.Watcher.Current().ByFileName(helpers.BinaryBaseName(binPath))
	if !ok || cfg.MaxProcs <= 0 {
		return 1
	}
	return cfg.MaxProcs
}

type tuningLifecycle struct {
	rpc rpc_tuning_rules.TuningRuleClient
}

func (l *tuningLifecycle) Ping(ctx context.Context) error {
	_, err := l.rpc.Ping(ctx, &rpc_tuning_rules.Empty{})
	return err
}

func (l *tuningLifecycle) Shutdown(ctx context.Context) error {
	_, err := l.rpc.Shutdown(ctx, &rpc_tuning_rules.Empty{})
	return err
}

type tuningPlugin struct{ goplugin.NetRPCUnsupportedPlugin }

func (p *tuningPlugin) GRPCServer(_ *goplugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *tuningPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_tuning_rules.NewTuningRuleClient(c), nil
}
