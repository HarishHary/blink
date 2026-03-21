package sdk

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

const (
	ProtocolVersion = 1
	MagicKey        = "BLINK_PLUGIN"
	MagicValue      = "matcher_v1"
	DefaultTimeout  = 5 * time.Second
)

// MatcherMetadata holds the static properties of a matcher, returned by MatcherPlugin.Metadata().
type MatcherMetadata struct {
	ID          string
	Name        string
	Description string
	Enabled     bool
	Global      bool
	Version     string
}

// MatcherPlugin is the interface that all matcher plugin binaries must implement.
// Embed sdk.BaseMatcher to get no-op defaults for Init and Shutdown.
type MatcherPlugin interface {
	Metadata() MatcherMetadata
	Init() error
	Match(ctx context.Context, event events.Event) (bool, errors.Error)
	Shutdown() error
}

// BaseMatcher provides no-op defaults for Init and Shutdown.
// Embed in your matcher struct to avoid implementing them when not needed.
type BaseMatcher struct{}

func (BaseMatcher) Init() error     { return nil }
func (BaseMatcher) Shutdown() error { return nil }

// server wraps a MatcherPlugin and serves the gRPC MatcherServer interface.
type server struct {
	rpc_matchers.UnimplementedMatcherServer
	matcher MatcherPlugin
}

func (s *server) GetMetadata(_ context.Context, _ *rpc_matchers.Empty) (*rpc_matchers.MatcherMetadata, error) {
	m := s.matcher.Metadata()
	return &rpc_matchers.MatcherMetadata{
		Id:          m.ID,
		Name:        m.Name,
		Description: m.Description,
		Enabled:     m.Enabled,
		Global:      m.Global,
		Version:     m.Version,
	}, nil
}

func (s *server) Init(_ context.Context, _ *rpc_matchers.Empty) (*rpc_matchers.Empty, error) {
	return &rpc_matchers.Empty{}, s.matcher.Init()
}

func (s *server) Match(ctx context.Context, req *rpc_matchers.MatchRequest) (*rpc_matchers.MatchResponse, error) {
	var event events.Event
	if err := json.Unmarshal(req.GetEvent().GetJson(), &event); err != nil {
		return nil, err
	}
	matched, err := s.matcher.Match(ctx, event)
	if err != nil {
		return nil, err
	}
	return &rpc_matchers.MatchResponse{Matched: matched}, nil
}

func (s *server) Ping(_ context.Context, _ *rpc_matchers.Empty) (*rpc_matchers.Empty, error) {
	return &rpc_matchers.Empty{}, nil
}

func (s *server) Shutdown(_ context.Context, _ *rpc_matchers.Empty) (*rpc_matchers.Empty, error) {
	return &rpc_matchers.Empty{}, s.matcher.Shutdown()
}

type pluginImpl struct {
	plugin.NetRPCUnsupportedPlugin
	matcher MatcherPlugin
}

func (p *pluginImpl) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	rpc_matchers.RegisterMatcherServer(s, &server{matcher: p.matcher})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_matchers.NewMatcherClient(c), nil
}

func Serve(m MatcherPlugin) {
	os.Setenv("GODEBUG", "madvdontneed=1")
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  ProtocolVersion,
			MagicCookieKey:   MagicKey,
			MagicCookieValue: MagicValue,
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Plugins: map[string]plugin.Plugin{
			"matcher": &pluginImpl{matcher: m},
		},
	})
}
