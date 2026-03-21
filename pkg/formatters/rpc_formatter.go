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
func (f *rpcFormatter) Checksum() string    { return f.checksum }
func (f *rpcFormatter) String() string {
	return fmt.Sprintf("Formatter '%s' (id:%s, enabled:%t)", f.meta.GetName(), f.meta.GetId(), f.meta.GetEnabled())
}

func (f *rpcFormatter) Format(ctx context.Context, alert *alerts.Alert) (map[string]any, errors.Error) {
	b, err := json.Marshal(alert)
	if err != nil {
		return nil, errors.NewE(err)
	}
	resp, err := f.client.Format(ctx, &rpc_formatters.FormatRequest{AlertJson: b})
	if err != nil {
		return nil, errors.NewE(err)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.GetResultJson(), &result); err != nil {
		return nil, errors.NewE(err)
	}
	return result, nil
}
