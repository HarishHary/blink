package tuning_rules

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

// NewTuningRuleAdapter builds the PluginAdapter for the tuning_rules plugin type.
func NewTuningRuleAdapter(manager *TuningRuleConfigManager) *plugin.PluginAdapter[TuningRule] {
	return &plugin.PluginAdapter[TuningRule]{
		Key:        "tuning_rule",
		Magic:      "tuning_rule_v1",
		Plugin:     &tuningPlugin{},
		Controller: manager,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (TuningRule, plugin.PluginLifecycle, string, string, error) {
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

			tr := newRpcTuningRule(fileName, rpc, manager, hash)
			id, name := fileName, fileName
			if desired, ok := manager.DesiredForBinary(fileName); ok {
				id = desired.ID
				name = desired.Name
			}
			return tr, &tuningLifecycle{rpc: rpc}, id, name, nil
		},
	}
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

type tuningPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
}

func (p *tuningPlugin) GRPCServer(_ *goplugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *tuningPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_tuning_rules.NewTuningRuleClient(c), nil
}
