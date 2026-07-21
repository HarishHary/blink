package matchers

import (
	"context"
	"os"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/handshake"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

// MagicValue is the matcher plugin handshake cookie value.
const MagicValue = "matcher_v1"

// Plugin is the interface matcher plugin binaries must implement.
type Plugin interface {
	Init() error
	Match(ctx context.Context, event events.Event) (bool, errors.Error)
	Shutdown() error
}

// BaseMatcher provides no-op Init and Shutdown defaults.
type BaseMatcher struct{}

func (BaseMatcher) Init() error     { return nil }
func (BaseMatcher) Shutdown() error { return nil }

type server struct {
	rpc_matchers.UnimplementedMatcherServer
	matcher Plugin
}

func (s *server) Init(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.matcher.Init()
}

func (s *server) MatchBatch(ctx context.Context, req *rpc_matchers.MatchBatchRequest) (*rpc_matchers.MatchBatchResponse, error) {
	results := make([]bool, len(req.GetEvents()))
	errs := make([]string, len(req.GetEvents()))
	for i, ev := range req.GetEvents() {
		event := events.Event(ev.AsMap())
		matched, err := s.matcher.Match(ctx, event)
		if err != nil {
			if s := errors.PluginErrorStatus(err); s.Code() != codes.InvalidArgument {
				return nil, s.Err()
			}
			errs[i] = err.Error()
			continue
		}
		results[i] = matched
	}
	return &rpc_matchers.MatchBatchResponse{Results: results, Errors: errs}, nil
}

func (s *server) Ping(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *server) Shutdown(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.matcher.Shutdown()
}

type pluginImpl struct {
	plugin.NetRPCUnsupportedPlugin
	matcher Plugin
}

func (p *pluginImpl) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	rpc_matchers.RegisterMatcherServer(s, &server{matcher: p.matcher})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return rpc_matchers.NewMatcherClient(c), nil
}

// Serve starts the matcher plugin gRPC server.
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
