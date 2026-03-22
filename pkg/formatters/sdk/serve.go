package sdk

import (
	"context"
	"encoding/json"
	"os"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

const (
	ProtocolVersion = 1
	MagicKey        = "BLINK_PLUGIN"
	MagicValue      = "formatter_v1"
)

type FormatterMetadata struct {
	ID          string
	Name        string
	Description string
	Enabled     bool
}

type FormatterPlugin interface {
	Metadata() FormatterMetadata
	Init() error
	Format(ctx context.Context, alert map[string]any) (map[string]any, errors.Error)
	Shutdown() error
}

// BaseFormatter provides no-op defaults for Init and Shutdown.
type BaseFormatter struct{}

func (BaseFormatter) Init() error     { return nil }
func (BaseFormatter) Shutdown() error { return nil }

type server struct {
	rpc_formatters.UnimplementedFormatterServer
	formatter FormatterPlugin
}

func (s *server) GetMetadata(_ context.Context, _ *rpc_formatters.Empty) (*rpc_formatters.FormatterMetadata, error) {
	m := s.formatter.Metadata()
	return &rpc_formatters.FormatterMetadata{
		Id:          m.ID,
		Name:        m.Name,
		Description: m.Description,
		Enabled:     m.Enabled,
	}, nil
}

func (s *server) Init(_ context.Context, _ *rpc_formatters.Empty) (*rpc_formatters.Empty, error) {
	return &rpc_formatters.Empty{}, s.formatter.Init()
}

func (s *server) Format(ctx context.Context, req *rpc_formatters.FormatRequest) (*rpc_formatters.FormatResponse, error) {
	var alert map[string]any
	if err := json.Unmarshal(req.GetAlertJson(), &alert); err != nil {
		return nil, err
	}
	result, err := s.formatter.Format(ctx, alert)
	if err != nil {
		return nil, err
	}
	b, err2 := json.Marshal(result)
	if err2 != nil {
		return nil, err2
	}
	return &rpc_formatters.FormatResponse{ResultJson: b}, nil
}

func (s *server) FormatBatch(ctx context.Context, req *rpc_formatters.FormatBatchRequest) (*rpc_formatters.FormatBatchResponse, error) {
	results := make([][]byte, 0, len(req.GetAlertJson()))
	for _, raw := range req.GetAlertJson() {
		var alert map[string]any
		if err := json.Unmarshal(raw, &alert); err != nil {
			return nil, err
		}
		result, err := s.formatter.Format(ctx, alert)
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

func (s *server) Ping(_ context.Context, _ *rpc_formatters.Empty) (*rpc_formatters.Empty, error) {
	return &rpc_formatters.Empty{}, nil
}

func (s *server) Shutdown(_ context.Context, _ *rpc_formatters.Empty) (*rpc_formatters.Empty, error) {
	return &rpc_formatters.Empty{}, s.formatter.Shutdown()
}

type pluginImpl struct {
	plugin.NetRPCUnsupportedPlugin
	formatter FormatterPlugin
}

func (p *pluginImpl) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	rpc_formatters.RegisterFormatterServer(s, &server{formatter: p.formatter})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_formatters.NewFormatterClient(c), nil
}

func Serve(f FormatterPlugin) {
	os.Setenv("GODEBUG", "madvdontneed=1")
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  ProtocolVersion,
			MagicCookieKey:   MagicKey,
			MagicCookieValue: MagicValue,
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Plugins: map[string]plugin.Plugin{
			"formatter": &pluginImpl{formatter: f},
		},
	})
}
