package tuning_rules

import (
	"context"
	"os"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/handshake"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

// MagicValue is this plugin type's cookie value; the shared cookie key and
// protocol version live in internal/handshake.
const MagicValue = "tuning_rule_v1"

// Plugin is the interface every tuning rule binary implements. Embed BaseTuningRule for no-op Init/Shutdown.
type Plugin interface {
	Init() error
	Tune(ctx context.Context, alert alerts.Alert) (bool, errors.Error)
	Shutdown() error
}

// BaseTuningRule provides no-op defaults for Init and Shutdown. Embed in your rule struct.
type BaseTuningRule struct{}

func (BaseTuningRule) Init() error     { return nil }
func (BaseTuningRule) Shutdown() error { return nil }

// server wraps a Plugin and serves the gRPC TuningRuleServer interface.
type server struct {
	rpc_tuning_rules.UnimplementedTuningRuleServer
	rule Plugin
}

func (s *server) Init(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.rule.Init()
}

func (s *server) TuneBatch(ctx context.Context, req *rpc_tuning_rules.TuneBatchRequest) (*rpc_tuning_rules.TuneBatchResponse, error) {
	items := make([]*rpc_tuning_rules.TuneItem, len(req.GetAlerts()))
	for i, raw := range req.GetAlerts() {
		item := &rpc_tuning_rules.TuneItem{}
		items[i] = item
		alert, err := alerts.Unmarshal(raw)
		if err != nil {
			return nil, errors.PluginErrorStatus(err).Err()
		}
		applies, err := s.rule.Tune(ctx, *alert)
		if err != nil {
			if s := errors.PluginErrorStatus(err); s.Code() != codes.InvalidArgument {
				return nil, s.Err()
			}
			item.Error = err.Error()
			continue
		}
		item.Applies = applies
	}
	return &rpc_tuning_rules.TuneBatchResponse{Items: items}, nil
}

func (s *server) Ping(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *server) Shutdown(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.rule.Shutdown()
}

type pluginImpl struct {
	plugin.NetRPCUnsupportedPlugin
	rule Plugin
}

func (p *pluginImpl) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	rpc_tuning_rules.RegisterTuningRuleServer(s, &server{rule: p.rule})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return rpc_tuning_rules.NewTuningRuleClient(c), nil
}

// Serve starts the plugin RPC server for a tuning rule binary.
func Serve(r Plugin) {
	os.Setenv("GODEBUG", "madvdontneed=1")
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  handshake.ProtocolVersion,
			MagicCookieKey:   handshake.CookieKey,
			MagicCookieValue: MagicValue,
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Plugins: map[string]plugin.Plugin{
			"tuning_rule": &pluginImpl{rule: r},
		},
	})
}
