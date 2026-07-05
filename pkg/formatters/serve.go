package formatters

import (
	"context"
	"encoding/json"
	"os"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/handshake"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

const MagicValue = "formatter_v1"

type Plugin interface {
	Init() error
	Format(ctx context.Context, alert alerts.Alert) (map[string]any, errors.Error)
	Shutdown() error
}

type BaseFormatter struct{}

func (BaseFormatter) Init() error     { return nil }
func (BaseFormatter) Shutdown() error { return nil }

type server struct {
	rpc_formatters.UnimplementedFormatterServer
	formatter Plugin
}

func (s *server) Init(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.formatter.Init()
}

func (s *server) FormatBatch(ctx context.Context, req *rpc_formatters.FormatBatchRequest) (*rpc_formatters.FormatBatchResponse, error) {
	results := make([][]byte, 0, len(req.GetAlerts()))
	for _, pa := range req.GetAlerts() {
		alert, err := alerts.ProtoToAlert(pa)
		if err != nil {
			return nil, err
		}
		result, err := s.formatter.Format(ctx, *alert)
		if err != nil {
			return nil, err
		}
		b, err2 := json.Marshal(result)
		if err2 != nil {
			return nil, err2
		}
		results = append(results, b)
	}
	return &rpc_formatters.FormatBatchResponse{ResultJson: results}, nil
}

func (s *server) Ping(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *server) Shutdown(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.formatter.Shutdown()
}

type pluginImpl struct {
	plugin.NetRPCUnsupportedPlugin
	formatter Plugin
}

func (p *pluginImpl) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	rpc_formatters.RegisterFormatterServer(s, &server{formatter: p.formatter})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_formatters.NewFormatterClient(c), nil
}

func Serve(f Plugin) {
	os.Setenv("GODEBUG", "madvdontneed=1")
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  handshake.ProtocolVersion,
			MagicCookieKey:   handshake.CookieKey,
			MagicCookieValue: MagicValue,
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Plugins: map[string]plugin.Plugin{
			"formatter": &pluginImpl{formatter: f},
		},
	})
}
