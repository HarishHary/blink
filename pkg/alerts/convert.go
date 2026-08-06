package alerts

import (
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/alerts/pb"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/scoring"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// Serialises an Alert to protobuf bytes for Kafka transport.
func Marshal(a *Alert) ([]byte, error) {
	p, err := AlertToProto(a)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(p)
}

// Deserialises protobuf bytes from Kafka into an Alert.
func Unmarshal(data []byte) (*Alert, error) {
	var p pb.Alert
	if err := proto.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return ProtoToAlert(&p)
}

// Converts an in-process Alert to its proto wire representation.
func AlertToProto(a *Alert) (*pb.Alert, error) {
	if a == nil {
		return nil, errors.New("nil alert")
	}

	eventStruct, err := structpb.NewStruct(a.Event)
	if err != nil {
		return nil, err
	}
	ruleProto, err := RuleMetadataToProto(a.Rule)
	if err != nil {
		return nil, err
	}
	return &pb.Alert{
		Id:                  a.Id,
		Attempts:            int32(a.Attempts),
		Cluster:             a.Cluster,
		CreatedNs:           a.Created.UnixNano(),
		DispatchedNs:        a.Dispatched.UnixNano(),
		Event:               eventStruct,
		Staged:              a.Staged,
		OutputsSent:         a.OutputsSent,
		EnrichmentsApplied:  a.EnrichmentsApplied,
		OverrideMergeByKeys: a.OverrideMergeByKeys,
		LogSource:           a.LogSource,
		LogType:             a.LogType,
		SourceEntity:        a.SourceEntity,
		SourceService:       a.SourceService,
		Confidence:          a.Confidence.String(),
		Severity:            a.Severity.String(),
		Rule:                ruleProto,
	}, nil
}

// Converts a proto Alert back to an in-process Alert
func ProtoToAlert(p *pb.Alert) (*Alert, error) {
	if p == nil {
		return nil, errors.New("nil protobuf alert")
	}

	var event events.Event
	if p.GetEvent() != nil {
		event = events.Event(p.GetEvent().AsMap())
	}
	conf, _ := scoring.ParseConfidence(p.GetConfidence())
	sev, _ := scoring.ParseSeverity(p.GetSeverity())

	rule, err := ProtoToRuleMetadata(p.GetRule())
	if err != nil {
		return nil, err
	}
	return &Alert{
		Id:                  p.GetId(),
		Attempts:            int(p.GetAttempts()),
		Cluster:             p.GetCluster(),
		Created:             time.Unix(0, p.GetCreatedNs()).UTC(),
		Dispatched:          time.Unix(0, p.GetDispatchedNs()).UTC(),
		Event:               event,
		Staged:              p.GetStaged(),
		OutputsSent:         p.GetOutputsSent(),
		EnrichmentsApplied:  p.GetEnrichmentsApplied(),
		OverrideMergeByKeys: p.GetOverrideMergeByKeys(),
		LogSource:           p.GetLogSource(),
		LogType:             p.GetLogType(),
		SourceEntity:        p.GetSourceEntity(),
		SourceService:       p.GetSourceService(),
		Confidence:          conf,
		Severity:            sev,
		Rule:                rule,
	}, nil
}

// Converts a *rules.RuleMetadata to its protobuf representation for embedding in an alert payload.
func RuleMetadataToProto(r *rules.RuleMetadata) (*pb.RuleMetadata, error) {
	if r == nil {
		return nil, errors.New("nil rule metadata")
	}
	return &pb.RuleMetadata{
		Id:              r.Id,
		Name:            r.Name,
		Description:     r.Description,
		Enabled:         r.Enabled,
		Version:         r.Version,
		DisplayName:     r.DisplayName,
		Severity:        r.Severity.String(),
		Confidence:      r.Confidence.String(),
		MergeByKeys:     r.MergeByKeys,
		MergeWindowMins: uint32(r.MergeWindowMins() / time.Minute),
		ReqSubkeys:      r.ReqSubkeys,
		Signal:          r.Signal,
		SignalThreshold: r.SignalThreshold.String(),
		Tags:            r.Tags,
		Dispatchers:     r.Dispatchers,
		LogTypes:        r.LogTypes,
		Matchers:        r.Matchers,
		Formatters:      r.Formatters,
		Enrichments:     r.Enrichments,
		TuningRules:     r.TuningRules,
		References:      r.References,
		RiskScore:       r.RiskScore.String(),
		Observables:     observablesToProto(r.Observables),
	}, nil
}

// Reconstructs a *rules.RuleMetadata from the alert's embedded rule metadata.
func ProtoToRuleMetadata(m *pb.RuleMetadata) (*rules.RuleMetadata, error) {
	if m == nil {
		return nil, errors.New("nil protobuf rule metadata")
	}
	cfg, err := rules.NewRuleMetadata(rules.RuleMetadata{
		PluginMetadata: plugin.PluginMetadata{
			Id:          m.GetId(),
			Name:        m.GetName(),
			DisplayName: m.GetDisplayName(),
			Description: m.GetDescription(),
			Enabled:     m.GetEnabled(),
			Version:     m.GetVersion(),
		},
		SeverityStr:          m.GetSeverity(),
		ConfidenceStr:        m.GetConfidence(),
		SignalThresholdStr:   m.GetSignalThreshold(),
		LogTypes:             m.GetLogTypes(),
		Matchers:             m.GetMatchers(),
		ReqSubkeys:           m.GetReqSubkeys(),
		MergeByKeys:          m.GetMergeByKeys(),
		MergeWindowMinsField: m.GetMergeWindowMins(),
		Signal:               m.GetSignal(),
		Tags:                 m.GetTags(),
		References:           m.GetReferences(),
		Dispatchers:          m.GetDispatchers(),
		Formatters:           m.GetFormatters(),
		Enrichments:          m.GetEnrichments(),
		TuningRules:          m.GetTuningRules(),
		Observables:          observablesFromProto(m.GetObservables()),
	})
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// observablesToProto / observablesFromProto convert rule observables between the Go struct and the wire.
func observablesToProto(in []rules.Observable) []*pb.Observable {
	if len(in) == 0 {
		return nil
	}
	out := make([]*pb.Observable, len(in))
	for i, o := range in {
		out[i] = &pb.Observable{Name: o.Name, Description: o.Description, Aggregation: o.Aggregation}
	}
	return out
}

func observablesFromProto(in []*pb.Observable) []rules.Observable {
	if len(in) == 0 {
		return nil
	}
	out := make([]rules.Observable, len(in))
	for i, o := range in {
		out[i] = rules.Observable{Name: o.GetName(), Description: o.GetDescription(), Aggregation: o.GetAggregation()}
	}
	return out
}
