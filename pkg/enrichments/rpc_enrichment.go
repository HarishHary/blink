package enrichments

import (
	"context"
	"encoding/json"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

type rpcEnrichment struct {
	meta     *rpc_enrichments.EnrichmentMetadata
	checksum string
	client   rpc_enrichments.EnrichmentClient
}

func newRpcEnrichment(meta *rpc_enrichments.EnrichmentMetadata, client rpc_enrichments.EnrichmentClient, checksum string) *rpcEnrichment {
	return &rpcEnrichment{meta: meta, checksum: checksum, client: client}
}

func (r *rpcEnrichment) Id() string {
	if id := r.meta.GetId(); id != "" {
		return id
	}
	return r.meta.GetName()
}
func (r *rpcEnrichment) Name() string        { return r.meta.GetName() }
func (r *rpcEnrichment) Description() string { return r.meta.GetDescription() }
func (r *rpcEnrichment) Enabled() bool       { return r.meta.GetEnabled() }
func (r *rpcEnrichment) Checksum() string    { return r.checksum }
func (r *rpcEnrichment) DependsOn() []string { return r.meta.GetDependsOn() }
func (r *rpcEnrichment) String() string {
	return "RpcEnrichment '" + r.meta.GetName() + "' id:'" + r.meta.GetId() + "'"
}

func (r *rpcEnrichment) Enrich(ctx context.Context, alert *alerts.Alert) errors.Error {
	b, err := json.Marshal(alert.Event)
	if err != nil {
		return errors.New(err)
	}
	resp, err := r.client.Enrich(ctx, &rpc_enrichments.EnrichRequest{
		Alert: &rpc_enrichments.Alert{Json: b},
	})
	if err != nil {
		return errors.New(err)
	}
	var enriched map[string]any
	if err := json.Unmarshal(resp.GetAlert().GetJson(), &enriched); err != nil {
		return errors.New(err)
	}
	for k, v := range enriched {
		alert.Event[k] = v
	}
	return nil
}
