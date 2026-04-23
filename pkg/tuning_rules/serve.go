package tuning_rules

import (
	"context"
	"encoding/json"
	"os"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

const (
	ProtocolVersion = 1
	MagicKey        = "BLINK_PLUGIN"
	MagicValue      = "tuning_rule_v1"
)

// TuningRulePlugin is the interface that all tuning rule plugin binaries must implement.
// Embed sdk.BaseTuningRule to get no-op defaults for Init and Shutdown.
//
// All static metadata (name, id, enabled, global, rule_type, confidence, etc.) lives in
// the YAML sidecar file alongside the binary — the subprocess owns only tuning logic.
type TuningRulePlugin interface {
	Init() error
	Tune(ctx context.Context, alert map[string]any) (bool, errors.Error)
	Shutdown() error
}

// BaseTuningRule provides no-op defaults for Init and Shutdown. Embed in your rule struct.
type BaseTuningRule struct{}

func (BaseTuningRule) Init() error     { return nil }
func (BaseTuningRule) Shutdown() error { return nil }

// server wraps a TuningRulePlugin and serves the gRPC TuningRuleServer interface.
type server struct {
	rpc_tuning_rules.UnimplementedTuningRuleServer
	rule TuningRulePlugin
}

func (s *server) Init(_ context.Context, _ *rpc_tuning_rules.Empty) (*rpc_tuning_rules.Empty, error) {
	return &rpc_tuning_rules.Empty{}, s.rule.Init()
}

func (s *server) TuneBatch(ctx context.Context, req *rpc_tuning_rules.TuneBatchRequest) (*rpc_tuning_rules.TuneBatchResponse, error) {
	results := make([]bool, 0, len(req.GetAlertJson()))
	for _, raw := range req.GetAlertJson() {
		var alert map[string]any
		if err := json.Unmarshal(raw, &alert); err != nil {
			return nil, err
		}
		applies, err := s.rule.Tune(ctx, alert)
		if err != nil {
			return nil, err
		}
		results = append(results, applies)
	}
	return &rpc_tuning_rules.TuneBatchResponse{Applies: results}, nil
}

func (s *server) Ping(_ context.Context, _ *rpc_tuning_rules.Empty) (*rpc_tuning_rules.Empty, error) {
	return &rpc_tuning_rules.Empty{}, nil
}

func (s *server) Shutdown(_ context.Context, _ *rpc_tuning_rules.Empty) (*rpc_tuning_rules.Empty, error) {
	return &rpc_tuning_rules.Empty{}, s.rule.Shutdown()
}

type pluginImpl struct {
	plugin.NetRPCUnsupportedPlugin
	rule TuningRulePlugin
}

func (p *pluginImpl) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	rpc_tuning_rules.RegisterTuningRuleServer(s, &server{rule: p.rule})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_tuning_rules.NewTuningRuleClient(c), nil
}

func Serve(r TuningRulePlugin) {
	os.Setenv("GODEBUG", "madvdontneed=1")
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  ProtocolVersion,
			MagicCookieKey:   MagicKey,
			MagicCookieValue: MagicValue,
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Plugins: map[string]plugin.Plugin{
			"tuning_rule": &pluginImpl{rule: r},
		},
	})
}
