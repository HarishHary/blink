package rules

import (
	"context"
	"encoding/json"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules/config"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
	"github.com/harishhary/blink/pkg/scoring"
)

// This is the executor-side wrapper for a live rule subprocess.
type rpcRule struct {
	client     rpc_rules.RuleClient
	cfgWatcher *config.Watcher
	fileName   string
	checksum   string // SHA-256 ofthe binary
}

func newRpcRule(fileName string, client rpc_rules.RuleClient, watcher *config.Watcher, checksum string) *rpcRule {
	return &rpcRule{
		client:     client,
		cfgWatcher: watcher,
		fileName:   fileName,
		checksum:   checksum,
	}
}

func (r *rpcRule) cfg() *config.RuleMetadata {
	if r.cfgWatcher == nil {
		return nil
	}
	return r.cfgWatcher.Current().ByFileName(r.fileName)
}

func (r *rpcRule) Id() string {
	if c := r.cfg(); c != nil {
		return c.Id()
	}
	return ""
}

func (r *rpcRule) Name() string {
	if c := r.cfg(); c != nil {
		return c.Name()
	}
	return r.fileName
}

func (r *rpcRule) Enabled() bool {
	c := r.cfg()
	return c != nil && c.Enabled()
}

func (r *rpcRule) Description() string {
	if c := r.cfg(); c != nil {
		return c.Description()
	}
	return ""
}

func (r *rpcRule) FileName() string {
	return r.fileName
}

func (r *rpcRule) DisplayName() string {
	if c := r.cfg(); c != nil {
		return c.DisplayName()
	}
	return ""
}

func (r *rpcRule) References() []string {
	if c := r.cfg(); c != nil {
		return c.References()
	}
	return nil
}

func (r *rpcRule) Severity() scoring.Severity {
	if c := r.cfg(); c != nil {
		return c.Severity()
	}
	return scoring.SeverityInfo
}

func (r *rpcRule) Confidence() scoring.Confidence {
	if c := r.cfg(); c != nil {
		return c.Confidence()
	}
	return scoring.ConfidenceVeryLow
}

func (r *rpcRule) RiskScore() scoring.RiskScore {
	if c := r.cfg(); c != nil {
		return c.RiskScore()
	}
	return scoring.ComputeRiskScore(scoring.ConfidenceVeryLow, scoring.SeverityInfo)
}

func (r *rpcRule) MergeByKeys() []string {
	if c := r.cfg(); c != nil {
		return c.MergeByKeys()
	}
	return nil
}

func (r *rpcRule) MergeWindowMins() time.Duration {
	if c := r.cfg(); c != nil {
		return c.MergeWindowMins()
	}
	return 0
}

func (r *rpcRule) ReqSubkeys() []string {
	if c := r.cfg(); c != nil {
		return c.ReqSubkeys()
	}
	return nil
}

func (r *rpcRule) Signal() bool {
	if c := r.cfg(); c != nil {
		return c.Signal()
	}
	return false
}

func (r *rpcRule) SignalThreshold() scoring.Confidence {
	if c := r.cfg(); c != nil {
		return c.SignalThreshold()
	}
	return scoring.ConfidenceVeryLow
}

func (r *rpcRule) Tags() []string {
	if c := r.cfg(); c != nil {
		return c.Tags()
	}
	return nil
}

func (r *rpcRule) Dispatchers() []string {
	if c := r.cfg(); c != nil {
		return c.Dispatchers()
	}
	return nil
}

func (r *rpcRule) LogTypes() []string {
	if c := r.cfg(); c != nil {
		return c.LogTypes()
	}
	return nil
}

func (r *rpcRule) Observables() []Observables {
	return nil
}

func (r *rpcRule) Matchers() []string {
	if c := r.cfg(); c != nil {
		return c.Matchers()
	}
	return nil
}

func (r *rpcRule) Formatters() []string {
	if c := r.cfg(); c != nil {
		return c.Formatters()
	}
	return nil
}

func (r *rpcRule) Enrichments() []string {
	if c := r.cfg(); c != nil {
		return c.Enrichments()
	}
	return nil
}

func (r *rpcRule) TuningRules() []string {
	if c := r.cfg(); c != nil {
		return c.TuningRules()
	}
	return nil
}

func (r *rpcRule) Version() string {
	if c := r.cfg(); c != nil {
		return c.Version()
	}
	return ""
}

func (r *rpcRule) Checksum() string {
	return r.checksum
}

// --- Optional capability interfaces ---

func (r *rpcRule) AlertTitle(_ events.Event) string {
	if c := r.cfg(); c != nil {
		return c.Name()
	}
	return r.fileName
}

func (r *rpcRule) AlertDescription(_ events.Event) string {
	if c := r.cfg(); c != nil {
		return c.Description()
	}
	return ""
}

func (r *rpcRule) AlertContext(_ events.Event) map[string]any {
	return nil
}

func (r *rpcRule) DynamicSeverity(_ events.Event) scoring.Severity {
	return r.Severity()
}

func (r *rpcRule) Dedup(_ events.Event) []string {
	return r.MergeByKeys()
}

// SubKeyFilter uses the YAML config (via cfg()) so the subprocess is not invoked.
func (r *rpcRule) SubKeysInEvent(event events.Event) bool {
	return DefaultSubKeysInEvent(r, event)
}

// ctx carries the caller's deadline (e.g. the executor's per-event timeout).
func (r *rpcRule) Evaluate(ctx context.Context, evts []events.Event) ([]bool, errors.Error) {
	protoEvents := make([]*rpc_rules.Event, 0, len(evts))
	for _, ev := range evts {
		b, err := json.Marshal(ev)
		if err != nil {
			return nil, errors.New(err)
		}
		protoEvents = append(protoEvents, &rpc_rules.Event{Json: b})
	}
	resp, err := r.client.EvaluateBatch(ctx, &rpc_rules.EvaluateBatchRequest{Events: protoEvents})
	if err != nil {
		return nil, errors.New(err)
	}
	return resp.GetMatched(), nil
}
