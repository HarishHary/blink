package tuning_rules

import (
	"context"
	"fmt"
	"time"

	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/pluginmgr"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

type TuningRuleAdapter struct{}

func (l *TuningRuleAdapter) PluginKey() string         { return "tuning_rule" }
func (l *TuningRuleAdapter) MagicValue() string        { return "tuning_rule_v1" }
func (l *TuningRuleAdapter) GRPCPlugin() plugin.Plugin { return &tuningPlugin{} }

func (l *TuningRuleAdapter) Handshake(ctx context.Context, raw interface{}, _ string, hash string) (TuningRule, pluginmgr.PluginLifecycle, string, string, error) {
	rpc, ok := raw.(rpc_tuning_rules.TuningRuleClient)
	if !ok {
		return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
	}

	metaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	meta, err := rpc.GetMetadata(metaCtx, &rpc_tuning_rules.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("metadata: %w", err)
	}

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = rpc.Init(initCtx, &rpc_tuning_rules.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("init: %w", err)
	}

	tr := newRpcTuningRule(meta, rpc, hash)
	return tr, &tuningLifecycle{rpc: rpc}, meta.GetId(), meta.GetName(), nil
}

// IsEnabled always returns true - tuning rules have no YAML sidecar.
func (l *TuningRuleAdapter) IsEnabled(_ *pluginmgr.PluginHandle) bool { return true }

// Workers always returns 1 - no YAML sidecar to configure parallelism.
func (l *TuningRuleAdapter) Workers(_ string) int { return 1 }

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

type tuningPlugin struct{ plugin.NetRPCUnsupportedPlugin }

func (p *tuningPlugin) GRPCServer(_ *plugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *tuningPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_tuning_rules.NewTuningRuleClient(c), nil
}
