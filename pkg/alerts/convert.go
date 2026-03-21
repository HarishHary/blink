package alerts

import (
	"time"

	"github.com/harishhary/blink/pkg/alerts/pb"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/rules/config"
	"github.com/harishhary/blink/pkg/scoring"
	proto "google.golang.org/protobuf/proto"
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
	eventStruct, err := structpb.NewStruct(a.Event)
	if err != nil {
		return nil, err
	}
	p := &pb.Alert{
		AlertId:       a.AlertID,
		Attempts:      int32(a.Attempts),
		Cluster:       a.Cluster,
		CreatedNs:     a.Created.UnixNano(),
		DispatchedNs:  a.Dispatched.UnixNano(),
		Event:         eventStruct,
		Staged:              a.Staged,
		OutputsSent:         a.OutputsSent,
		EnrichmentsApplied:  a.EnrichmentsApplied,
		LogSource:     a.LogSource,
		LogType:       a.LogType,
		SourceEntity:  a.SourceEntity,
		SourceService: a.SourceService,
		Confidence:    a.Confidence.String(),
		Severity:      a.Severity.String(),
		Rule:          ruleToProto(a.Rule),
	}
	return p, nil
}

// Converts a proto Alert back to an in-process Alert
func ProtoToAlert(p *pb.Alert) (*Alert, error) {
	var event events.Event
	if p.GetEvent() != nil {
		event = events.Event(p.GetEvent().AsMap())
	}
	conf, _ := scoring.ParseConfidence(p.GetConfidence())
	sev, _ := scoring.ParseSeverity(p.GetSeverity())

	a := &Alert{
		AlertID:       p.GetAlertId(),
		Attempts:      int(p.GetAttempts()),
		Cluster:       p.GetCluster(),
		Created:       time.Unix(0, p.GetCreatedNs()).UTC(),
		Dispatched:    time.Unix(0, p.GetDispatchedNs()).UTC(),
		Event:         event,
		Staged:             p.GetStaged(),
		OutputsSent:        p.GetOutputsSent(),
		EnrichmentsApplied: p.GetEnrichmentsApplied(),
		LogSource:     p.GetLogSource(),
		LogType:       p.GetLogType(),
		SourceEntity:  p.GetSourceEntity(),
		SourceService: p.GetSourceService(),
		Confidence:    conf,
		Severity:      sev,
		Rule:          protoToRuleMetadata(p.GetRule()),
	}
	return a, nil
}

// Converts a Metadata value to its protobuf representation for embedding in an alert payload.
func ruleToProto(r rules.Metadata) *pb.RuleMetadata {
	if r == nil {
		return nil
	}
	return &pb.RuleMetadata{
		Id:              r.Id(),
		Name:            r.Name(),
		Description:     r.Description(),
		Enabled:         r.Enabled(),
		Severity:        r.Severity().String(),
		Confidence:      r.Confidence().String(),
		MergeByKeys:     r.MergeByKeys(),
		MergeWindowMins: uint32(r.MergeWindowMins() / time.Minute),
		ReqSubkeys:      r.ReqSubkeys(),
		Signal:          r.Signal(),
		SignalThreshold: r.SignalThreshold().String(),
		Tags:            r.Tags(),
		Dispatchers:     r.Dispatchers(),
		LogTypes:        r.LogTypes(),
		Matchers:        r.Matchers(),
		Formatters:      r.Formatters(),
		Enrichments:     r.Enrichments(),
		TuningRules:     r.TuningRules(),
		Version:         r.Version(),
		Checksum:        r.Checksum(),
		FileName:        r.FileName(),
		DisplayName:     r.DisplayName(),
		References:      r.References(),
	}
}

// Reconstructs a *config.RuleMetadata from the alert's embedded rule metadata.
func protoToRuleMetadata(m *pb.RuleMetadata) *config.RuleMetadata {
	if m == nil {
		return &config.RuleMetadata{}
	}
	cfg, _ := config.New(config.RuleMetadata{
		IDField:              m.GetId(),
		NameField:            m.GetName(),
		DisplayNameField:     m.GetDisplayName(),
		DescriptionField:     m.GetDescription(),
		EnabledField:         m.GetEnabled(),
		VersionField:         m.GetVersion(),
		FileNameField:        m.GetFileName(),
		ChecksumField:        m.GetChecksum(),
		SeverityStr:          m.GetSeverity(),
		ConfidenceStr:        m.GetConfidence(),
		SignalThresholdStr:   m.GetSignalThreshold(),
		LogTypesField:        m.GetLogTypes(),
		MatchersField:        m.GetMatchers(),
		ReqSubkeysField:      m.GetReqSubkeys(),
		MergeByKeysField:     m.GetMergeByKeys(),
		MergeWindowMinsField: m.GetMergeWindowMins(),
		SignalField:          m.GetSignal(),
		TagsField:            m.GetTags(),
		ReferencesField:      m.GetReferences(),
		DispatchersField:     m.GetDispatchers(),
		FormattersField:      m.GetFormatters(),
		EnrichmentsField:     m.GetEnrichments(),
		TuningRulesField:     m.GetTuningRules(),
	})
	return cfg
}
