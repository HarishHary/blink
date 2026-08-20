package formatters

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	pb "github.com/harishhary/blink/pkg/alerts/pb"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

type rpcFormatter struct {
	metadata FormatterMetadata
	fileName string
	checksum string
	client   rpc_formatters.FormatterClient
}

func newRpcFormatter(fileName string, client rpc_formatters.FormatterClient, metadata FormatterMetadata, checksum string) *rpcFormatter {
	return &rpcFormatter{
		metadata: metadata,
		fileName: fileName,
		checksum: checksum,
		client:   client,
	}
}

// FormatterMetadata returns an independently owned snapshot-derived formatter configuration.
func (r *rpcFormatter) FormatterMetadata() *FormatterMetadata {
	return r.metadata.Clone()
}

func (r *rpcFormatter) Metadata() plugin.Spec {
	return r.FormatterMetadata().Metadata()
}

func (r *rpcFormatter) Checksum() string { return r.checksum }

func (r *rpcFormatter) FormatBatch(ctx context.Context, batch []*alerts.Alert) FormatResult {
	pbAlerts := make([]*pb.Alert, 0, len(batch))
	for _, a := range batch {
		pa, err := alerts.AlertToProto(a)
		if err != nil {
			return FormatResult{CallErr: errors.NewE(err)}
		}
		pbAlerts = append(pbAlerts, pa)
	}
	resp, err := r.client.FormatBatch(ctx, &rpc_formatters.FormatBatchRequest{Alerts: pbAlerts})
	if err != nil {
		return FormatResult{CallErr: errors.NewE(err)}
	}
	if resp == nil {
		return FormatResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "formatter", PluginID: r.fileName, Field: "response", Expected: 1})}
	}
	if len(resp.GetItems()) != len(batch) {
		return FormatResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "formatter", PluginID: r.fileName, Field: "items", Expected: len(batch), Actual: len(resp.GetItems())})}
	}
	items := make([]FormatItem, len(batch))
	for i, item := range resp.GetItems() {
		if item.GetError() != "" {
			items[i].Err = errors.New(item.GetError())
		}
		if items[i].Err != nil {
			continue
		}
		var result map[string]any
		if err := json.Unmarshal(item.GetResultJson(), &result); err != nil {
			return FormatResult{CallErr: errors.NewE(fmt.Errorf("decode formatter %q result for alert %q: %w", r.fileName, batch[i].Id, err))}
		}
		items[i].Output = result
	}
	return FormatResult{Items: items}
}
