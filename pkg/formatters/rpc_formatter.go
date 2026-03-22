package formatters

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

type rpcFormatter struct {
	meta     *rpc_formatters.FormatterMetadata
	checksum string
	client   rpc_formatters.FormatterClient
}

func newRpcFormatter(meta *rpc_formatters.FormatterMetadata, client rpc_formatters.FormatterClient, checksum string) *rpcFormatter {
	return &rpcFormatter{meta: meta, checksum: checksum, client: client}
}

func (f *rpcFormatter) Id() string {
	if id := f.meta.GetId(); id != "" {
		return id
	}
	return f.meta.GetName()
}
func (f *rpcFormatter) Name() string        { return f.meta.GetName() }
func (f *rpcFormatter) Description() string { return f.meta.GetDescription() }
func (f *rpcFormatter) Enabled() bool       { return f.meta.GetEnabled() }
func (f *rpcFormatter) Version() string     { return f.meta.GetVersion() }
func (f *rpcFormatter) Checksum() string    { return f.checksum }
func (f *rpcFormatter) String() string {
	return fmt.Sprintf("Formatter '%s' (id:%s, enabled:%t)", f.meta.GetName(), f.meta.GetId(), f.meta.GetEnabled())
}

func (f *rpcFormatter) Format(ctx context.Context, alrts []*alerts.Alert) ([]map[string]any, errors.Error) {
	alertJSONs := make([][]byte, 0, len(alrts))
	for _, alrt := range alrts {
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
