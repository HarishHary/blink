package matchers

import (
	"context"
	"encoding/json"
	"os"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/handshake"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

const MagicValue = "matcher_v1"

type Plugin interface {
	Init() error
	Match(ctx context.Context, event events.Event) (bool, errors.Error)
	Shutdown() error
}

type BaseMatcher struct{}

func (BaseMatcher) Init() error     { return nil }
func (BaseMatcher) Shutdown() error { return nil }

type server struct {
	rpc_matchers.UnimplementedMatcherServer
	matcher Plugin
}

func (s *server) Init(_ context.Context, _ *rpc_matchers.Empty) (*rpc_matchers.Empty, error) {
	return &rpc_matchers.Empty{}, s.matcher.Init()
}

func (s *server) MatchBatch(ctx context.Context, req *rpc_matchers.MatchBatchRequest) (*rpc_matchers.MatchBatchResponse, error) {
	results := make([]bool, 0, len(req.GetEvents()))
	for _, ev := range req.GetEvents() {
		var event events.Event
		if err := json.Unmarshal(ev.GetJson(), &event); err != nil {
			return nil, err
		}
		matched, err := s.matcher.Match(ctx, event)
		if err != nil {
			return nil, err
		}
		results = append(results, matched)
	}
	return &rpc_matchers.MatchBatchResponse{Matched: results}, nil
}

func (s *server) Ping(_ context.Context, _ *rpc_matchers.Empty) (*rpc_matchers.Empty, error) {
	return &rpc_matchers.Empty{}, nil
}

func (s *server) Shutdown(_ context.Context, _ *rpc_matchers.Empty) (*rpc_matchers.Empty, error) {
	return &rpc_matchers.Empty{}, s.matcher.Shutdown()
}

type pluginImpl struct {
	plugin.NetRPCUnsupportedPlugin
	matcher Plugin
}

func (p *pluginImpl) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	rpc_matchers.RegisterMatcherServer(s, &server{matcher: p.matcher})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_matchers.NewMatcherClient(c), nil
}

func Serve(m Plugin) {
	os.Setenv("GODEBUG", "madvdontneed=1")
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  handshake.ProtocolVersion,
			MagicCookieKey:   handshake.CookieKey,
			MagicCookieValue: MagicValue,
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Plugins: map[string]plugin.Plugin{
			"matcher": &pluginImpl{matcher: m},
		},
	})
}
