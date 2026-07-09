package rules

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

type rpcRule struct {
	cfg      config.Source[*RuleMetadata]
	fileName string
	checksum string
	client   rpc_rules.RuleClient
}

func newRpcRule(fileName string, client rpc_rules.RuleClient, cfg config.Source[*RuleMetadata], checksum string) *rpcRule {
	return &rpcRule{
		cfg:      cfg,
		fileName: fileName,
		checksum: checksum,
		client:   client,
	}
}

func (r *rpcRule) config() *RuleMetadata {
	if r.cfg == nil {
		return nil
	}
	v, ok := r.cfg.ByFileName(r.fileName)
	if !ok {
		return nil
	}
	return v
}

// RuleMetadata returns the snapshot-derived rule configuration for this plugin.
func (r *rpcRule) RuleMetadata() *RuleMetadata {
	if c := r.config(); c != nil {
		return c
	}
	// Return a minimal stub so callers don't need to nil-check.
	return &RuleMetadata{PluginMetadata: plugin.PluginMetadata{Id: r.fileName, Name: r.fileName}}
}

func (r *rpcRule) Metadata() plugin.PluginMetadata {
	if c := r.config(); c != nil {
		return c.Metadata()
	}
	return plugin.PluginMetadata{Id: r.fileName, Name: r.fileName}
}

func (r *rpcRule) Checksum() string { return r.checksum }

// ctx carries the caller's deadline (e.g. the executor's per-event timeout).
func (r *rpcRule) Evaluate(ctx context.Context, evts []events.Event) ([]EventResult, errors.Error) {
	protoEvents := make([]*structpb.Struct, 0, len(evts))
	for _, ev := range evts {
		s, err := structpb.NewStruct(ev)
		if err != nil {
			return nil, errors.NewE(err)
		}
		protoEvents = append(protoEvents, s)
	}
	resp, err := r.client.EvaluateBatch(ctx, &rpc_rules.EvaluateBatchRequest{Events: protoEvents})
	if err != nil {
		return nil, errors.NewE(err)
	}

	out := make([]EventResult, len(resp.GetResults()))
	for i, r := range resp.GetResults() {
		res := EventResult{
			Matched:     r.GetMatched(),
			Title:       r.GetTitle(),
			Description: r.GetDescription(),
			Severity:    r.GetSeverity(),
			MergeByKeys: r.GetMergeByKeys(),
		}
		if c := r.GetContext(); c != nil {
			res.Context = c.AsMap()
		}
		out[i] = res
	}
	return out, nil
}
