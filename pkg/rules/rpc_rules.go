package rules

import (
	"context"
	"encoding/json"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules/config"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

// This is the executor-side wrapper for a live rule subprocess.
type rpcRule struct {
	client     rpc_rules.RuleClient
	cfgWatcher *config.Watcher
	fileName   string
	checksum   string // SHA-256 of the binary
}

func newRpcRule(fileName string, client rpc_rules.RuleClient, watcher *config.Watcher, checksum string) *rpcRule {
	return &rpcRule{
		client:     client,
		cfgWatcher: watcher,
		fileName:   fileName,
		checksum:   checksum,
	}
}

func (r *rpcRule) cfg() *config.RuleMetadata {
	if r.cfgWatcher == nil {
		return nil
	}
	return r.cfgWatcher.Current().ByFileName(r.fileName)
}

// RuleMetadata returns the live YAML-derived rule configuration for this plugin.
func (r *rpcRule) RuleMetadata() *config.RuleMetadata {
	if c := r.cfg(); c != nil {
		return c
	}
	// Return a minimal stub so callers don't need to nil-check.
	return &config.RuleMetadata{FileNameField: r.fileName}
}

func (r *rpcRule) Checksum() string { return r.checksum }

func (r *rpcRule) PluginMetadata() plugin.PluginMetadata {
	if c := r.cfg(); c != nil {
		return c.PluginMetadata()
	}
	return plugin.PluginMetadata{Name: r.fileName}
}

// ctx carries the caller's deadline (e.g. the executor's per-event timeout).
func (r *rpcRule) Evaluate(ctx context.Context, evts []events.Event) ([]EvalResult, errors.Error) {
	protoEvents := make([]*rpc_rules.Event, 0, len(evts))
	for _, ev := range evts {
		b, err := json.Marshal(ev)
		if err != nil {
			return nil, errors.New(err)
		}
		protoEvents = append(protoEvents, &rpc_rules.Event{Json: b})
	}
	resp, err := r.client.EvaluateBatch(ctx, &rpc_rules.EvaluateBatchRequest{Events: protoEvents})
	if err != nil {
		return nil, errors.New(err)
	}

	out := make([]EvalResult, len(resp.GetResults()))
	for i, r := range resp.GetResults() {
		res := EvalResult{
			Matched:     r.GetMatched(),
			Title:       r.GetTitle(),
			Description: r.GetDescription(),
			Severity:    r.GetSeverity(),
			MergeByKeys: r.GetMergeByKeys(),
		}
		if b := r.GetContextJson(); len(b) > 0 {
			var ctx map[string]any
			if err := json.Unmarshal(b, &ctx); err == nil {
				res.Context = ctx
			}
		}
		out[i] = res
	}
	return out, nil
}
