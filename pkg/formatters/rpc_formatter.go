package formatters

import (
	"context"
	"encoding/json"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	pb "github.com/harishhary/blink/pkg/alerts/pb"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

type rpcFormatter struct {
	cfg      config.Source[*FormatterMetadata]
	fileName string
	checksum string
	client   rpc_formatters.FormatterClient
}

func newRpcFormatter(fileName string, client rpc_formatters.FormatterClient, cfg config.Source[*FormatterMetadata], checksum string) *rpcFormatter {
	return &rpcFormatter{
		cfg:      cfg,
		fileName: fileName,
		checksum: checksum,
		client:   client,
	}
}

func (f *rpcFormatter) config() *FormatterMetadata {
	if f.cfg == nil {
		return nil
	}
	v, _ := f.cfg.ByFileName(f.fileName)
	return v
}

// FormatterMetadata returns the live YAML-derived formatter configuration.
func (f *rpcFormatter) FormatterMetadata() *FormatterMetadata {
	if c := f.config(); c != nil {
		return c
	}
	return &FormatterMetadata{PluginMetadata: plugin.PluginMetadata{Id: f.fileName, Name: f.fileName}}
}

func (f *rpcFormatter) Metadata() plugin.PluginMetadata {
	if c := f.config(); c != nil {
		return c.Metadata()
	}
	return plugin.PluginMetadata{Id: f.fileName, Name: f.fileName}
}

func (f *rpcFormatter) Checksum() string { return f.checksum }

func (f *rpcFormatter) Format(ctx context.Context, batch []*alerts.Alert) ([]map[string]any, errors.Error) {
	pbAlerts := make([]*pb.Alert, 0, len(batch))
	for _, a := range batch {
		pa, err := alerts.AlertToProto(a)
		if err != nil {
			return nil, errors.NewE(err)
		}
		pbAlerts = append(pbAlerts, pa)
	}
	resp, err := f.client.FormatBatch(ctx, &rpc_formatters.FormatBatchRequest{Alerts: pbAlerts})
	if err != nil {
		return nil, errors.NewE(err)
	}
	results := make([]map[string]any, len(resp.GetResultJson()))
	for i, raw := range resp.GetResultJson() {
		var result map[string]any
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, errors.NewE(err)
		}
		results[i] = result
	}
	return results, nil
}
