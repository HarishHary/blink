package tuning_rules

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

type rpcTuningRule struct {
	cfgManager *TuningRuleConfigManager
	fileName   string
	checksum   string
	client     rpc_tuning_rules.TuningRuleClient
}

func newRpcTuningRule(fileName string, client rpc_tuning_rules.TuningRuleClient, manager *TuningRuleConfigManager, checksum string) *rpcTuningRule {
	return &rpcTuningRule{
		cfgManager: manager,
		fileName:   fileName,
		checksum:   checksum,
		client:     client,
	}
}

func (r *rpcTuningRule) cfg() *TuningRuleMetadata {
	if r.cfgManager == nil {
		return nil
	}
	v, _ := r.cfgManager.Current().ByFileName(r.fileName)
	return v
}

// TuningMetadata returns the live YAML-derived tuning rule configuration.
func (r *rpcTuningRule) TuningRuleMetadata() *TuningRuleMetadata {
	if c := r.cfg(); c != nil {
		return c
	}
	return &TuningRuleMetadata{PluginMetadata: plugin.PluginMetadata{Id: r.fileName, Name: r.fileName}}
}

func (r *rpcTuningRule) Metadata() plugin.PluginMetadata {
	return r.TuningRuleMetadata().Metadata()
}

func (r *rpcTuningRule) Checksum() string { return r.checksum }
func (r *rpcTuningRule) String() string {
	m := r.TuningRuleMetadata().Metadata()
	return fmt.Sprintf("TuningRule '%s' (id:%s, enabled:%t)", m.Name, m.Id, m.Enabled)
}

func (r *rpcTuningRule) Global() bool { return r.TuningRuleMetadata().Global }

// RuleType parses the YAML rule_type string into a typed RuleType constant.
func (r *rpcTuningRule) RuleType() RuleType {
	switch r.TuningRuleMetadata().RuleType {
	case "set_confidence":
		return SetConfidence
	case "increase_confidence":
		return IncreaseConfidence
	case "decrease_confidence":
		return DecreaseConfidence
	default:
		return Ignore
	}
}

// Confidence parses the YAML confidence string into a scoring.Confidence value.
func (r *rpcTuningRule) Confidence() scoring.Confidence {
	conf, _ := scoring.ParseConfidence(r.TuningRuleMetadata().Confidence)
	return conf
}

func (r *rpcTuningRule) Tune(ctx context.Context, alerts []alerts.Alert) ([]bool, errors.Error) {
	alertJSONs := make([][]byte, 0, len(alerts))
	for _, alrt := range alerts {
		b, err := json.Marshal(alrt)
		if err != nil {
			return nil, errors.NewE(err)
		}
		alertJSONs = append(alertJSONs, b)
	}
	resp, err := r.client.TuneBatch(ctx, &rpc_tuning_rules.TuneBatchRequest{AlertJson: alertJSONs})
	if err != nil {
		return nil, errors.NewE(err)
	}
	return resp.GetApplies(), nil
}
