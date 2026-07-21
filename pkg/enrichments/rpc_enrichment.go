package enrichments

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	pb "github.com/harishhary/blink/pkg/alerts/pb"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

type rpcEnrichment struct {
	cfg      config.Source[*EnrichmentMetadata]
	fileName string
	checksum string
	client   rpc_enrichments.EnrichmentClient
}

func newRpcEnrichment(fileName string, client rpc_enrichments.EnrichmentClient, cfg config.Source[*EnrichmentMetadata], checksum string) *rpcEnrichment {
	return &rpcEnrichment{
		cfg:      cfg,
		fileName: fileName,
		checksum: checksum,
		client:   client,
	}
}

func (r *rpcEnrichment) config() *EnrichmentMetadata {
	if r.cfg == nil {
		return nil
	}
	v, _ := r.cfg.ByFileName(r.fileName)
	return v
}

// EnrichmentMetadata returns the live YAML-derived enrichment configuration.
func (r *rpcEnrichment) EnrichmentMetadata() *EnrichmentMetadata {
	if c := r.config(); c != nil {
		return c
	}
	return &EnrichmentMetadata{PluginMetadata: plugin.PluginMetadata{Id: r.fileName, Name: r.fileName}}
}

func (r *rpcEnrichment) Metadata() plugin.PluginMetadata {
	if c := r.config(); c != nil {
		return c.Metadata()
	}
	return plugin.PluginMetadata{Id: r.fileName, Name: r.fileName}
}

func (r *rpcEnrichment) Checksum() string { return r.checksum }

func (r *rpcEnrichment) EnrichBatch(ctx context.Context, batch []*alerts.Alert) EnrichResult {
	pbAlerts := make([]*pb.Alert, 0, len(batch))
	for _, a := range batch {
		pa, err := alerts.AlertToProto(a)
		if err != nil {
			return EnrichResult{CallErr: errors.NewE(err)}
		}
		pbAlerts = append(pbAlerts, pa)
	}
	resp, err := r.client.EnrichBatch(ctx, &rpc_enrichments.EnrichBatchRequest{Alerts: pbAlerts})
	if err != nil {
		return EnrichResult{CallErr: errors.NewE(err)}
	}
	if resp == nil {
		return EnrichResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "enrichment", PluginID: r.fileName, Field: "response", Expected: 1})}
	}
	if len(resp.GetResultJson()) != len(batch) {
		return EnrichResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "enrichment", PluginID: r.fileName, Field: "results", Expected: len(batch), Actual: len(resp.GetResultJson())})}
	}
	if len(resp.GetErrors()) != len(batch) {
		return EnrichResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "enrichment", PluginID: r.fileName, Field: "errors", Expected: len(batch), Actual: len(resp.GetErrors())})}
	}

	perErrs := make([]errors.Error, len(batch))
	for i, message := range resp.GetErrors() {
		if message != "" {
			perErrs[i] = errors.New(message)
		}
	}
	enrichedAlerts := make([]map[string]any, len(batch))
	for i, raw := range resp.GetResultJson() {
		if perErrs[i] != nil {
			continue
		}
		var enriched map[string]any
		if err := json.Unmarshal(raw, &enriched); err != nil {
			return EnrichResult{CallErr: errors.NewE(fmt.Errorf("decode enrichment %q result for alert %q: %w", r.fileName, batch[i].Id, err))}
		}
		enrichedAlerts[i] = enriched
	}
	for i, enriched := range enrichedAlerts {
		maps.Copy(batch[i].Event, enriched)
	}
	return EnrichResult{Errs: perErrs}
}
