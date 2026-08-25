package rules

import (
	"context"
	"os"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/handshake"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

// MagicValue is the handshake cookie value for rule plugins.
const MagicValue = "rule_v1"

// Plugin defines the SDK contract implemented by rule binaries.
type Plugin interface {
	// Init prepares the plugin before it begins serving evaluations.
	Init() error
	// Evaluate returns whether the event matched and any rule-specific error.
	Evaluate(ctx context.Context, event events.Event) (bool, errors.Error)
	// Shutdown releases plugin resources before process exit.
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

// BaseRule provides no-op defaults for optional Plugin methods.
type BaseRule struct{}

// Init is a no-op default implementation.
func (BaseRule) Init() error { return nil }

// Shutdown is a no-op default implementation.
func (BaseRule) Shutdown() error { return nil }

// AlertTitle returns no title override by default.
func (BaseRule) AlertTitle(_ events.Event) string { return "" }

// AlertDescription returns no description override by default.
func (BaseRule) AlertDescription(_ events.Event) string { return "" }

// AlertSeverity returns no severity override by default.
func (BaseRule) AlertSeverity(_ events.Event) string { return "" }

// AlertContext returns no additional alert context by default.
func (BaseRule) AlertContext(_ events.Event) map[string]any { return nil }

// AlertMergeByKeys returns no merge key override by default.
func (BaseRule) AlertMergeByKeys(_ events.Event) []string { return nil }

// AlertReqSubkeys always allows evaluation by default.
func (BaseRule) AlertReqSubkeys(_ events.Event) bool { return true }

// server wraps a Plugin and serves the gRPC RuleServer interface.
type server struct {
	rpc_rules.UnimplementedRuleServer
	rule Plugin
}

func (s *server) Init(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.rule.Init()
}

func (s *server) EvaluateBatch(ctx context.Context, req *rpc_rules.EvaluateBatchRequest) (*rpc_rules.EvaluateBatchResponse, error) {
	items := make([]*rpc_rules.EvaluateItem, len(req.GetEvents()))
	for i, raw := range req.GetEvents() {
		item := &rpc_rules.EvaluateItem{}
		items[i] = item
		var value structpb.Struct
		if err := proto.Unmarshal(raw, &value); err != nil {
			item.Error = err.Error()
			continue
		}
		event := events.Event(value.AsMap())

		if !s.rule.AlertReqSubkeys(event) {
			continue
		}

		matched, err := s.rule.Evaluate(ctx, event)
		if err != nil {
			if status := errors.PluginErrorStatus(err); status.Code() != codes.InvalidArgument {
				return nil, status.Err()
			}
			item.Error = err.Error()
			continue
		}

		item.Matched = matched
		if matched {
			item.Title = s.rule.AlertTitle(event)
			item.Description = s.rule.AlertDescription(event)
			item.Severity = s.rule.AlertSeverity(event)
			item.MergeByKeys = s.rule.AlertMergeByKeys(event)
			if c := s.rule.AlertContext(event); len(c) > 0 {
				if st, err := structpb.NewStruct(c); err == nil {
					item.Context = st
				}
			}
		}
	}
	return &rpc_rules.EvaluateBatchResponse{Items: items}, nil
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

func (p *pluginImpl) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return rpc_rules.NewRuleClient(c), nil
}

// Serve starts a rule plugin process with the rules gRPC contract.
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
