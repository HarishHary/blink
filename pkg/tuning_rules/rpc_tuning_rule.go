package tuning_rules

import (
	"context"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	pb "github.com/harishhary/blink/pkg/alerts/pb"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

type rpcTuningRule struct {
	cfg      config.Source[*TuningRuleMetadata]
	fileName string
	checksum string
	client   rpc_tuning_rules.TuningRuleClient
}

func newRpcTuningRule(fileName string, client rpc_tuning_rules.TuningRuleClient, cfg config.Source[*TuningRuleMetadata], checksum string) *rpcTuningRule {
	return &rpcTuningRule{
		cfg:      cfg,
		fileName: fileName,
		checksum: checksum,
		client:   client,
	}
}

func (r *rpcTuningRule) config() *TuningRuleMetadata {
	if r.cfg == nil {
		return nil
	}
	v, _ := r.cfg.ByFileName(r.fileName)
	return v
}

// TuningRuleMetadata returns live config or a file-name fallback when config is unavailable.
func (r *rpcTuningRule) TuningRuleMetadata() *TuningRuleMetadata {
	if c := r.config(); c != nil {
		return c
	}
	return &TuningRuleMetadata{PluginMetadata: plugin.PluginMetadata{Id: r.fileName, Name: r.fileName}}
}

// Metadata returns the live plugin metadata, or a file-name fallback before config is available.
func (r *rpcTuningRule) Metadata() plugin.PluginMetadata {
	if c := r.config(); c != nil {
		return c.Metadata()
	}
	return plugin.PluginMetadata{Id: r.fileName, Name: r.fileName}
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
