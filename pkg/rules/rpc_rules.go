package rules

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

type rpcRule struct {
	metadata RuleMetadata
	fileName string
	checksum string
	client   rpc_rules.RuleClient
}

func newRpcRule(fileName string, client rpc_rules.RuleClient, metadata RuleMetadata, checksum string) *rpcRule {
	return &rpcRule{
		metadata: *metadata.Clone(),
		fileName: fileName,
		checksum: checksum,
		client:   client,
	}
}

// RuleMetadata returns an independently owned snapshot-derived rule configuration.
func (r *rpcRule) RuleMetadata() *RuleMetadata {
	return r.metadata.Clone()
}

func (r *rpcRule) Metadata() plugin.Spec {
	return r.RuleMetadata().Metadata()
}

func (r *rpcRule) Checksum() string { return r.checksum }

// ctx carries the caller's deadline (e.g. the executor's per-event timeout).
func (r *rpcRule) EvaluateBatch(ctx context.Context, evts []events.Event) EvaluateResult {
	protoEvents := make([]*structpb.Struct, 0, len(evts))
	for _, ev := range evts {
		s, err := structpb.NewStruct(ev)
		if err != nil {
			return EvaluateResult{CallErr: errors.NewE(err)}
		}
		protoEvents = append(protoEvents, s)
	}
	resp, err := r.client.EvaluateBatch(ctx, &rpc_rules.EvaluateBatchRequest{Events: protoEvents})
	if err != nil {
		return EvaluateResult{CallErr: errors.NewE(err)}
	}
	if resp == nil {
		return EvaluateResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "rule", PluginID: r.fileName, Field: "response", Expected: 1})}
	}
	if len(resp.GetItems()) != len(evts) {
		return EvaluateResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "rule", PluginID: r.fileName, Field: "items", Expected: len(evts), Actual: len(resp.GetItems())})}
	}

	items := make([]EvaluateItem, len(resp.GetItems()))
	for i, r := range resp.GetItems() {
		items[i] = EvaluateItem{
			Matched:     r.GetMatched(),
			Title:       r.GetTitle(),
			Description: r.GetDescription(),
			Severity:    r.GetSeverity(),
			MergeByKeys: r.GetMergeByKeys(),
		}
		if c := r.GetContext(); c != nil {
			items[i].Context = c.AsMap()
		}
		if r.GetError() != "" {
			items[i].Err = errors.New(r.GetError())
		}
	}
	return EvaluateResult{Items: items}
}
