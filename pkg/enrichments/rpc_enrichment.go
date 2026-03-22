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
func (r *rpcEnrichment) Version() string     { return r.meta.GetVersion() }
func (r *rpcEnrichment) Checksum() string    { return r.checksum }
func (r *rpcEnrichment) DependsOn() []string { return r.meta.GetDependsOn() }
func (r *rpcEnrichment) String() string {
	return "RpcEnrichment '" + r.meta.GetName() + "' id:'" + r.meta.GetId() + "'"
}

func (r *rpcEnrichment) Enrich(ctx context.Context, alrts []*alerts.Alert) errors.Error {
	protoAlerts := make([]*rpc_enrichments.Alert, 0, len(alrts))
	for _, alrt := range alrts {
		b, err := json.Marshal(alrt.Event)
		if err != nil {
			return errors.New(err)
		}
		protoAlerts = append(protoAlerts, &rpc_enrichments.Alert{Json: b})
	}
	resp, err := r.client.EnrichBatch(ctx, &rpc_enrichments.EnrichBatchRequest{Alerts: protoAlerts})
	if err != nil {
		return errors.New(err)
	}
	for i, a := range resp.GetAlerts() {
		var enriched map[string]any
		if err := json.Unmarshal(a.GetJson(), &enriched); err != nil {
			return errors.New(err)
		}
		for k, v := range enriched {
			alrts[i].Event[k] = v
		}
	}
	return nil
}
