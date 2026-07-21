package formatters

import (
	"context"
	"encoding/json"
	"fmt"

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

func (f *rpcFormatter) FormatBatch(ctx context.Context, batch []*alerts.Alert) FormatResult {
	pbAlerts := make([]*pb.Alert, 0, len(batch))
	for _, a := range batch {
		pa, err := alerts.AlertToProto(a)
		if err != nil {
			return FormatResult{CallErr: errors.NewE(err)}
		}
		pbAlerts = append(pbAlerts, pa)
	}
	resp, err := f.client.FormatBatch(ctx, &rpc_formatters.FormatBatchRequest{Alerts: pbAlerts})
	if err != nil {
		return FormatResult{CallErr: errors.NewE(err)}
	}
	if resp == nil {
		return FormatResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "formatter", PluginID: f.fileName, Field: "response", Expected: 1})}
	}
	if len(resp.GetResultJson()) != len(batch) {
		return FormatResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "formatter", PluginID: f.fileName, Field: "results", Expected: len(batch), Actual: len(resp.GetResultJson())})}
	}
	if len(resp.GetErrors()) != len(batch) {
		return FormatResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "formatter", PluginID: f.fileName, Field: "errors", Expected: len(batch), Actual: len(resp.GetErrors())})}
	}
	results := make([]map[string]any, len(batch))
	perErrs := make([]errors.Error, len(batch))
	for i, message := range resp.GetErrors() {
		if message != "" {
			perErrs[i] = errors.New(message)
		}
	}
	for i, raw := range resp.GetResultJson() {
		if perErrs[i] != nil {
			continue
		}
		var result map[string]any
		if err := json.Unmarshal(raw, &result); err != nil {
			return FormatResult{CallErr: errors.NewE(fmt.Errorf("decode formatter %q result for alert %q: %w", f.fileName, batch[i].Id, err))}
		}
		results[i] = result
	}
	return FormatResult{Outs: results, Errs: perErrs}
}
