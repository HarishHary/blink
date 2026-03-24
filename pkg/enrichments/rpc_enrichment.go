package enrichments

import (
	"context"
	"encoding/json"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments/config"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

type rpcEnrichment struct {
	cfgWatcher *config.Watcher
	fileName   string
	checksum   string
	client     rpc_enrichments.EnrichmentClient
}

func newRpcEnrichment(fileName string, client rpc_enrichments.EnrichmentClient, watcher *config.Watcher, checksum string) *rpcEnrichment {
	return &rpcEnrichment{
		cfgWatcher: watcher,
		fileName:   fileName,
		checksum:   checksum,
		client:     client,
	}
}

func (r *rpcEnrichment) cfg() *config.EnrichmentMetadata {
	if r.cfgWatcher == nil {
		return nil
	}
	v, _ := r.cfgWatcher.Current().ByFileName(r.fileName)
	return v
}

// EnrichmentMetadata returns the live YAML-derived enrichment configuration.
func (r *rpcEnrichment) EnrichmentMetadata() *config.EnrichmentMetadata {
	if c := r.cfg(); c != nil {
		return c
	}
	return &config.EnrichmentMetadata{PluginMetadata: plugin.PluginMetadata{Id: r.fileName, Name: r.fileName, FileName: r.fileName}}
}

func (r *rpcEnrichment) Metadata() plugin.PluginMetadata {
	return r.EnrichmentMetadata().Metadata()
}

func (r *rpcEnrichment) DependsOn() []string { return r.EnrichmentMetadata().DependsOn }
func (r *rpcEnrichment) Checksum() string    { return r.checksum }
func (r *rpcEnrichment) String() string {
	m := r.EnrichmentMetadata().Metadata()
	return "RpcEnrichment '" + m.Name + "' id:'" + m.Id + "'"
}

func (r *rpcEnrichment) Enrich(ctx context.Context, alerts []*alerts.Alert) errors.Error {
	protoAlerts := make([]*rpc_enrichments.Alert, 0, len(alerts))
	for _, alrt := range alerts {
		b, err := json.Marshal(alrt)
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
			alerts[i].Event[k] = v
		}
	}
	return nil
}
