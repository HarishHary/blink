package formatters

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters/config"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

type rpcFormatter struct {
	cfgWatcher *config.Watcher
	fileName   string
	checksum   string
	client     rpc_formatters.FormatterClient
}

func newRpcFormatter(fileName string, client rpc_formatters.FormatterClient, watcher *config.Watcher, checksum string) *rpcFormatter {
	return &rpcFormatter{
		cfgWatcher: watcher,
		fileName:   fileName,
		checksum:   checksum,
		client:     client,
	}
}

func (f *rpcFormatter) cfg() *config.FormatterMetadata {
	if f.cfgWatcher == nil {
		return nil
	}
	v, _ := f.cfgWatcher.Current().ByFileName(f.fileName)
	return v
}

// FormatterMetadata returns the live YAML-derived formatter configuration.
func (f *rpcFormatter) FormatterMetadata() *config.FormatterMetadata {
	if c := f.cfg(); c != nil {
		return c
	}
	return &config.FormatterMetadata{PluginMetadata: plugin.PluginMetadata{Id: f.fileName, Name: f.fileName, FileName: f.fileName}}
}

func (f *rpcFormatter) Metadata() plugin.PluginMetadata {
	return f.FormatterMetadata().Metadata()
}

func (f *rpcFormatter) Checksum() string { return f.checksum }
func (f *rpcFormatter) String() string {
	m := f.FormatterMetadata().Metadata()
	return fmt.Sprintf("Formatter '%s' (id:%s)", m.Name, m.Id)
}

func (f *rpcFormatter) Format(ctx context.Context, alerts []*alerts.Alert) ([]map[string]any, errors.Error) {
	alertJSONs := make([][]byte, 0, len(alerts))
	for _, alrt := range alerts {
		b, err := json.Marshal(alrt)
		if err != nil {
			return nil, errors.NewE(err)
		}
		alertJSONs = append(alertJSONs, b)
	}
	resp, err := f.client.FormatBatch(ctx, &rpc_formatters.FormatBatchRequest{AlertJson: alertJSONs})
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
