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
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

const (
	ProtocolVersion = 1
	MagicKey        = "BLINK_PLUGIN"
	MagicValue      = "rule_v1"
	DefaultTimeout  = 5 * time.Second
)

// RulePlugin is the interface that all rule plugin binaries must implement.
// Embed sdk.BaseRule to get no-op defaults for Init and Shutdown.
// All rule metadata (name, severity, log_types, etc.) lives in the YAML
// sidecar file alongside the binary - the subprocess only owns Evaluate.
type RulePlugin interface {
	// Init is called once after the plugin connects, before any Evaluate calls.
	// Use it to compile regexes, load ML models, or open connections.
	Init() error
	Evaluate(ctx context.Context, event events.Event) (bool, errors.Error)
	Shutdown() error
}

// BaseRule provides no-op defaults for Init and Shutdown.
// Embed in your rule struct to avoid implementing them when not needed.
type BaseRule struct{}

func (BaseRule) Init() error     { return nil }
func (BaseRule) Shutdown() error { return nil }

// server wraps a RulePlugin and serves the gRPC RuleServer interface.
type server struct {
	rpc_rules.UnimplementedRuleServer
	rule RulePlugin
}

func (s *server) Init(_ context.Context, _ *rpc_rules.Empty) (*rpc_rules.Empty, error) {
	return &rpc_rules.Empty{}, s.rule.Init()
}

func (s *server) Evaluate(ctx context.Context, req *rpc_rules.EvaluateRequest) (*rpc_rules.EvaluateResponse, error) {
	var event events.Event
	if err := json.Unmarshal(req.GetEvent().GetJson(), &event); err != nil {
		return nil, err
	}
	matched, err := s.rule.Evaluate(ctx, event)
	if err != nil {
		return nil, err
	}
	return &rpc_rules.EvaluateResponse{Matched: matched}, nil
}

func (s *server) EvaluateBatch(ctx context.Context, req *rpc_rules.EvaluateBatchRequest) (*rpc_rules.EvaluateBatchResponse, error) {
	results := make([]bool, 0, len(req.GetEvents()))
	for _, ev := range req.GetEvents() {
		var event events.Event
		if err := json.Unmarshal(ev.GetJson(), &event); err != nil {
			return nil, err
		}
		matched, err := s.rule.Evaluate(ctx, event)
		if err != nil {
			return nil, err
		}
		results = append(results, matched)
	}
	return &rpc_rules.EvaluateBatchResponse{Matched: results}, nil
}

func (s *server) Ping(_ context.Context, _ *rpc_rules.Empty) (*rpc_rules.Empty, error) {
	return &rpc_rules.Empty{}, nil
}

func (s *server) Shutdown(_ context.Context, _ *rpc_rules.Empty) (*rpc_rules.Empty, error) {
	return &rpc_rules.Empty{}, s.rule.Shutdown()
}

type pluginImpl struct {
	plugin.NetRPCUnsupportedPlugin
	rule RulePlugin
}

func (p *pluginImpl) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	rpc_rules.RegisterRuleServer(s, &server{rule: p.rule})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_rules.NewRuleClient(c), nil
}

func Serve(r RulePlugin) {
	os.Setenv("GODEBUG", "madvdontneed=1")
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  ProtocolVersion,
			MagicCookieKey:   MagicKey,
			MagicCookieValue: MagicValue,
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Plugins: map[string]plugin.Plugin{
			"rule": &pluginImpl{rule: r},
		},
	})
}
