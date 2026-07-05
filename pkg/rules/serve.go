package rules

import (
	"context"
	"os"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/handshake"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

const MagicValue = "rule_v1"

type Plugin interface {
	Init() error
	Evaluate(ctx context.Context, event events.Event) (bool, errors.Error)
	Shutdown() error

	// AlertTitle returns a dynamic title for the alert.
	// Return "" to use the YAML display_name (default).
	AlertTitle(event events.Event) string

	// AlertDescription returns a dynamic description for the alert.
	// Return "" to use the YAML description (default).
	AlertDescription(event events.Event) string

	// AlertSeverity returns an event-level severity override.
	// Return one of: "info", "low", "medium", "high", "critical", or "" to use the YAML value (default).
	AlertSeverity(event events.Event) string

	// AlertContext returns extra key-value pairs merged into the alert event.
	// Return nil to add nothing.
	AlertContext(event events.Event) map[string]any

	// AlertMergeByKeys returns the merge keys for this event, overriding YAML merge_by_keys.
	// Return nil to use the YAML value (default).
	AlertMergeByKeys(event events.Event) []string

	// AlertReqSubkeys guards evaluation: return false to skip Evaluate for this event.
	// Useful for dynamic field presence checks beyond the static req_subkeys in YAML.
	// Return true to always evaluate (default).
	AlertReqSubkeys(event events.Event) bool
}

// BaseRule provides pass-through / no-op defaults for all Plugin methods.
// Embed in your rule struct and override only what you need.
type BaseRule struct{}

func (BaseRule) Init() error                                { return nil }
func (BaseRule) Shutdown() error                            { return nil }
func (BaseRule) AlertTitle(_ events.Event) string           { return "" }
func (BaseRule) AlertDescription(_ events.Event) string     { return "" }
func (BaseRule) AlertSeverity(_ events.Event) string        { return "" }
func (BaseRule) AlertContext(_ events.Event) map[string]any { return nil }
func (BaseRule) AlertMergeByKeys(_ events.Event) []string   { return nil }
func (BaseRule) AlertReqSubkeys(_ events.Event) bool        { return true }

// server wraps a Plugin and serves the gRPC RuleServer interface.
type server struct {
	rpc_rules.UnimplementedRuleServer
	rule Plugin
}

func (s *server) Init(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.rule.Init()
}

func (s *server) EvaluateBatch(ctx context.Context, req *rpc_rules.EvaluateBatchRequest) (*rpc_rules.EvaluateBatchResponse, error) {
	results := make([]*rpc_rules.EventResult, 0, len(req.GetEvents()))
	for _, ev := range req.GetEvents() {
		event := events.Event(ev.AsMap())

		if !s.rule.AlertReqSubkeys(event) {
			results = append(results, &rpc_rules.EventResult{Matched: false})
			continue
		}

		matched, err := s.rule.Evaluate(ctx, event)
		if err != nil {
			return nil, err
		}

		result := &rpc_rules.EventResult{Matched: matched}
		if matched {
			result.Title = s.rule.AlertTitle(event)
			result.Description = s.rule.AlertDescription(event)
			result.Severity = s.rule.AlertSeverity(event)
			result.MergeByKeys = s.rule.AlertMergeByKeys(event)
			if c := s.rule.AlertContext(event); len(c) > 0 {
				if st, err := structpb.NewStruct(c); err == nil {
					result.Context = st
				}
			}
		}
		results = append(results, result)
	}
	return &rpc_rules.EvaluateBatchResponse{Results: results}, nil
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
	rpc_rules.RegisterRuleServer(s, &server{rule: p.rule})
	return nil
}

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_rules.NewRuleClient(c), nil
}

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
			"rule": &pluginImpl{rule: r},
		},
	})
}
