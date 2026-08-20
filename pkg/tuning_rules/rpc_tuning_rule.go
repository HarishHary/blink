package tuning_rules

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	pb "github.com/harishhary/blink/pkg/alerts/pb"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

type rpcTuningRule struct {
	metadata TuningRuleMetadata
	fileName string
	checksum string
	client   rpc_tuning_rules.TuningRuleClient
}

func newRpcTuningRule(fileName string, client rpc_tuning_rules.TuningRuleClient, metadata TuningRuleMetadata, checksum string) *rpcTuningRule {
	return &rpcTuningRule{
		metadata: metadata,
		fileName: fileName,
		checksum: checksum,
		client:   client,
	}
}

// TuningRuleMetadata returns an independently owned snapshot-derived configuration.
func (r *rpcTuningRule) TuningRuleMetadata() *TuningRuleMetadata {
	return r.metadata.Clone()
}

// Metadata returns the snapshot-derived plugin metadata.
func (r *rpcTuningRule) Metadata() plugin.Spec {
	return r.TuningRuleMetadata().Metadata()
}

// Checksum returns the binary checksum captured during handshake.
func (r *rpcTuningRule) Checksum() string { return r.checksum }

// Tune converts alerts to protobufs, invokes the RPC batch method, and returns per-alert apply decisions.
func (r *rpcTuningRule) TuneBatch(ctx context.Context, batch []alerts.Alert) TuneResult {
	pbAlerts := make([]*pb.Alert, 0, len(batch))
	for i := range batch {
		pa, err := alerts.AlertToProto(&batch[i])
		if err != nil {
			return TuneResult{CallErr: errors.NewE(err)}
		}
		pbAlerts = append(pbAlerts, pa)
	}
	resp, err := r.client.TuneBatch(ctx, &rpc_tuning_rules.TuneBatchRequest{Alerts: pbAlerts})
	if err != nil {
		return TuneResult{CallErr: errors.NewE(err)}
	}
	if resp == nil {
		return TuneResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "tuning rule", PluginID: r.fileName, Field: "response", Expected: 1})}
	}
	if len(resp.GetItems()) != len(batch) {
		return TuneResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "tuning rule", PluginID: r.fileName, Field: "items", Expected: len(batch), Actual: len(resp.GetItems())})}
	}
	items := make([]TuneItem, len(batch))
	for i, item := range resp.GetItems() {
		items[i].Applies = item.GetApplies()
		if item.GetError() != "" {
			items[i].Err = errors.New(item.GetError())
		}
	}
	return TuneResult{Items: items}
}
