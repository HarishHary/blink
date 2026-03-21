package sdk

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

// TuningMetadata holds the static properties returned by TuningRulePlugin.Metadata().
type TuningMetadata struct {
	ID          string
	Name        string
	Description string
	Enabled     bool
	Global      bool
	RuleType    int32 // 0=Ignore, 1=SetConfidence, 2=IncreaseConfidence, 3=DecreaseConfidence
	Confidence  string
}

type TuningRulePlugin interface {
	Metadata() TuningMetadata
	Init() error
	Tune(ctx context.Context, alert map[string]any) (bool, errors.Error)
	Shutdown() error
}

type BaseTuningRule struct{}

func (BaseTuningRule) Init() error     { return nil }
func (BaseTuningRule) Shutdown() error { return nil }

// server wraps a TuningRulePlugin and serve the gRPC TuningRuleServer interface.
type server struct {
	rpc_tuning_rules.UnimplementedTuningRuleServer
	rule TuningRulePlugin
}

func (s *server) GetMetadata(_ context.Context, _ *rpc_tuning_rules.Empty) (*rpc_tuning_rules.TuningMetadata, error) {
	m := s.rule.Metadata()
	return &rpc_tuning_rules.TuningMetadata{
		Id:          m.ID,
		Name:        m.Name,
		Description: m.Description,
		Enabled:     m.Enabled,
		Global:      m.Global,
		RuleType:    m.RuleType,
		Confidence:  m.Confidence,
	}, nil
}

func (s *server) Init(_ context.Context, _ *rpc_tuning_rules.Empty) (*rpc_tuning_rules.Empty, error) {
	return &rpc_tuning_rules.Empty{}, s.rule.Init()
}

func (s *server) Tune(ctx context.Context, req *rpc_tuning_rules.TuneRequest) (*rpc_tuning_rules.TuneResponse, error) {
	var alert map[string]any
	if err := json.Unmarshal(req.GetAlertJson(), &alert); err != nil {
		return nil, err
	}
	applies, err := s.rule.Tune(ctx, alert)
	if err != nil {
		return nil, err
	}
	return &rpc_tuning_rules.TuneResponse{Applies: applies}, nil
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
