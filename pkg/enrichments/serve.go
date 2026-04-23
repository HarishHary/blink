package enrichments

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

const (
	ProtocolVersion = 1
	MagicKey        = "BLINK_PLUGIN"
	MagicValue      = "enrichment_v1"
	DefaultTimeout  = 5 * time.Second
)

// EnrichmentPlugin is the interface that all enrichment plugin binaries must implement.
// Embed sdk.BaseEnrichment to get no-op defaults for Init and Shutdown.
//
// All static metadata (name, id, enabled, depends_on, etc.) lives in the YAML
// sidecar file alongside the binary - the subprocess owns only enrichment logic.
type EnrichmentPlugin interface {
	// Init is called once after the plugin connects, before any Enrich calls.
	Init() error
	// Enrich enriches the alert event fields and returns the modified fields.
	Enrich(ctx context.Context, alert map[string]any) (map[string]any, errors.Error)
	// Shutdown is called before the plugin exits. Release any held resources.
	Shutdown() error
}

// BaseEnrichment provides no-op defaults for Init and Shutdown.
// Embed in your enrichment struct to avoid implementing them when not needed.
type BaseEnrichment struct{}

func (BaseEnrichment) Init() error     { return nil }
func (BaseEnrichment) Shutdown() error { return nil }

type server struct {
	rpc_enrichments.UnimplementedEnrichmentServer
	enrichment EnrichmentPlugin
}

func (s *server) Init(_ context.Context, _ *rpc_enrichments.Empty) (*rpc_enrichments.Empty, error) {
	return &rpc_enrichments.Empty{}, s.enrichment.Init()
}

func (s *server) EnrichBatch(ctx context.Context, req *rpc_enrichments.EnrichBatchRequest) (*rpc_enrichments.EnrichBatchResponse, error) {
	results := make([]*rpc_enrichments.Alert, 0, len(req.GetAlerts()))
	for _, a := range req.GetAlerts() {
		var alert map[string]any
		if err := json.Unmarshal(a.GetJson(), &alert); err != nil {
			return nil, err
		}
		enriched, err := s.enrichment.Enrich(ctx, alert)
		if err != nil {
			return nil, err
		}
		b, err2 := json.Marshal(enriched)
		if err2 != nil {
			return nil, err2
		}
		results = append(results, &rpc_enrichments.Alert{Json: b})
	}
	return &rpc_enrichments.EnrichBatchResponse{Alerts: results}, nil
}

func (s *server) Ping(_ context.Context, _ *rpc_enrichments.Empty) (*rpc_enrichments.Empty, error) {
	return &rpc_enrichments.Empty{}, nil
}

func (s *server) Shutdown(_ context.Context, _ *rpc_enrichments.Empty) (*rpc_enrichments.Empty, error) {
	return &rpc_enrichments.Empty{}, s.enrichment.Shutdown()
}

type pluginImpl struct {
	plugin.NetRPCUnsupportedPlugin
	enrichment EnrichmentPlugin
}

func (p *pluginImpl) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	rpc_enrichments.RegisterEnrichmentServer(s, &server{enrichment: p.enrichment})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_enrichments.NewEnrichmentClient(c), nil
}

func Serve(e EnrichmentPlugin) {
	os.Setenv("GODEBUG", "madvdontneed=1")
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  ProtocolVersion,
			MagicCookieKey:   MagicKey,
			MagicCookieValue: MagicValue,
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Plugins: map[string]plugin.Plugin{
			"enrichment": &pluginImpl{enrichment: e},
		},
	})
}
