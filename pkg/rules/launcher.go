package rules

import (
	"context"
	"fmt"
	"time"

	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/pluginmgr"
	"github.com/harishhary/blink/pkg/rules/config"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

type RuleAdapter struct {
	Watcher *config.Watcher
}

func (l *RuleAdapter) PluginKey() string         { return "rule" }
func (l *RuleAdapter) MagicValue() string        { return "rule_v1" }
func (l *RuleAdapter) GRPCPlugin() plugin.Plugin { return &rulePlugin{} }

// Connects to the rule subprocess, reads the YAML sidecar for its metadata, calls Init, and returns a ready rpcRule. The rule binary's basename must match the YAML file_name field.
func (l *RuleAdapter) Handshake(ctx context.Context, raw interface{}, binPath string, hash string) (Rule, pluginmgr.PluginLifecycle, string, string, error) {
	rpc, ok := raw.(rpc_rules.RuleClient)
	if !ok {
		return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
	}

	fileName := helpers.BinaryBaseName(binPath)
	cfg := l.Watcher.Current().ByFileName(fileName)
	if cfg == nil {
		return nil, nil, "", "", fmt.Errorf("rule launcher: no YAML sidecar found for binary %q (looked up file_name=%q)", binPath, fileName)
	}

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := rpc.Init(initCtx, &rpc_rules.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("init: %w", err)
	}

	rule := newRpcRule(fileName, rpc, l.Watcher, hash)
	return rule, &ruleLifecycle{rpc: rpc}, cfg.Id(), cfg.Name(), nil
}

// IsEnabled reports whether the rule's YAML sidecar still exists and is enabled.
// Called during every reconcile func so process-zombies (binary running but YAML removed/disabled) are stopped without waiting for a binary change.
func (l *RuleAdapter) IsEnabled(h *pluginmgr.PluginHandle) bool {
	cfg := l.Watcher.Current().ByFileName(helpers.BinaryBaseName(h.BinPath))
	return cfg != nil && cfg.Enabled()
}

func (l *RuleAdapter) Workers(binPath string) int {
	cfg := l.Watcher.Current().ByFileName(helpers.BinaryBaseName(binPath))
	if cfg == nil || cfg.MaxProcs() <= 0 {
		return 1
	}
	return cfg.MaxProcs()
}

type ruleLifecycle struct {
	rpc rpc_rules.RuleClient
}

func (l *ruleLifecycle) Ping(ctx context.Context) error {
	_, err := l.rpc.Ping(ctx, &rpc_rules.Empty{})
	return err
}

func (l *ruleLifecycle) Shutdown(ctx context.Context) error {
	_, err := l.rpc.Shutdown(ctx, &rpc_rules.Empty{})
	return err
}

// rulePlugin is the go-plugin client-side stub.
type rulePlugin struct{ plugin.NetRPCUnsupportedPlugin }

func (p *rulePlugin) GRPCServer(_ *plugin.GRPCBroker, _ *grpc.Server) error {
	return nil
}
func (p *rulePlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_rules.NewRuleClient(c), nil
}
