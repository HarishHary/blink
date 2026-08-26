package enrichments

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

type rpcEnrichment struct {
	metadata EnrichmentMetadata
	fileName string
	checksum string
	client   rpc_enrichments.EnrichmentClient
}

func newRpcEnrichment(fileName string, client rpc_enrichments.EnrichmentClient, metadata EnrichmentMetadata, checksum string) *rpcEnrichment {
	return &rpcEnrichment{
		metadata: *metadata.Clone(),
		fileName: fileName,
		checksum: checksum,
		client:   client,
	}
}

// EnrichmentMetadata returns an independently owned snapshot-derived enrichment configuration.
func (r *rpcEnrichment) EnrichmentMetadata() *EnrichmentMetadata {
	return r.metadata.Clone()
}

func (r *rpcEnrichment) Metadata() plugin.Spec {
	return r.EnrichmentMetadata().Metadata()
}

func (r *rpcEnrichment) Checksum() string { return r.checksum }

// EnrichBatch sends the batch's encodings and applies each returned enrichment to the alert the call
// carried, which is the caller's copy to keep or discard by the error beside it.
func (r *rpcEnrichment) EnrichBatch(ctx context.Context, batch *alerts.Batch) EnrichResult {
	resp, err := r.client.EnrichBatch(ctx, &rpc_enrichments.EnrichBatchRequest{Alerts: batch.Raw()})
	if err != nil {
		return EnrichResult{CallErr: errors.NewE(err)}
	}
	if resp == nil {
		return EnrichResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "enrichment", PluginID: r.fileName, Field: "response", Expected: 1})}
	}
	if len(resp.GetItems()) != batch.Len() {
		return EnrichResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "enrichment", PluginID: r.fileName, Field: "items", Expected: batch.Len(), Actual: len(resp.GetItems())})}
	}

	perErrs := make([]errors.Error, batch.Len())
	for i, item := range resp.GetItems() {
		if item.GetError() != "" {
			perErrs[i] = errors.New(item.GetError())
		}
	}
	enrichedAlerts := make([]map[string]any, batch.Len())
	for i, item := range resp.GetItems() {
		if perErrs[i] != nil {
			continue
		}
		var enriched map[string]any
		if err := json.Unmarshal(item.GetResultJson(), &enriched); err != nil {
			return EnrichResult{CallErr: errors.NewE(fmt.Errorf("decode enrichment %q result for alert %q: %w", r.fileName, batch.At(i).Id, err))}
		}
		enrichedAlerts[i] = enriched
	}
	for i, enriched := range enrichedAlerts {
		maps.Copy(batch.At(i).Event, enriched)
	}
	return EnrichResult{Errs: perErrs}
}
