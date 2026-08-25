package enrichments

import (
	"context"
	"encoding/json"
	"os"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/handshake"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

// MagicValue is the enrichment plugin handshake cookie value.
const MagicValue = "enrichment_v1"

// Plugin is the interface enrichment plugin binaries must implement.
type Plugin interface {
	Init() error
	// Enrich returns enrichment fields for a single alert.
	Enrich(ctx context.Context, alert alerts.Alert) (map[string]any, errors.Error)
	Shutdown() error
}

// BaseEnrichment provides no-op Init and Shutdown defaults.
type BaseEnrichment struct{}

func (BaseEnrichment) Init() error     { return nil }
func (BaseEnrichment) Shutdown() error { return nil }

type server struct {
	rpc_enrichments.UnimplementedEnrichmentServer
	enrichment Plugin
}

func (s *server) Init(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.enrichment.Init()
}

func (s *server) EnrichBatch(ctx context.Context, req *rpc_enrichments.EnrichBatchRequest) (*rpc_enrichments.EnrichBatchResponse, error) {
	items := make([]*rpc_enrichments.EnrichItem, len(req.GetAlerts()))
	for i, raw := range req.GetAlerts() {
		item := &rpc_enrichments.EnrichItem{}
		items[i] = item
		alert, err := alerts.Unmarshal(raw)
		if err != nil {
			item.Error = err.Error()
			continue
		}
		enriched, err := s.enrichment.Enrich(ctx, *alert)
		if err != nil {
			if status := errors.PluginErrorStatus(err); status.Code() != codes.InvalidArgument {
				return nil, status.Err()
			}
			item.Error = err.Error()
			continue
		}
		b, err := json.Marshal(enriched)
		if err != nil {
			item.Error = err.Error()
			continue
		}
		item.ResultJson = b
	}
	return &rpc_enrichments.EnrichBatchResponse{Items: items}, nil
}

func (s *server) Ping(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *server) Shutdown(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.enrichment.Shutdown()
}

type pluginImpl struct {
	plugin.NetRPCUnsupportedPlugin
	enrichment Plugin
}

func (p *pluginImpl) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	rpc_enrichments.RegisterEnrichmentServer(s, &server{enrichment: p.enrichment})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return rpc_enrichments.NewEnrichmentClient(c), nil
}

// Serve starts the enrichment plugin gRPC server.
func Serve(e Plugin) {
	os.Setenv("GODEBUG", "madvdontneed=1")
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  handshake.ProtocolVersion,
			MagicCookieKey:   handshake.CookieKey,
			MagicCookieValue: MagicValue,
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Plugins: map[string]plugin.Plugin{
			"enrichment": &pluginImpl{enrichment: e},
		},
	})
}
