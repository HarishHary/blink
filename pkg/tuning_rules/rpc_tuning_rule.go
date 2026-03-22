package tuning_rules

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

type rpcTuningRule struct {
	meta     *rpc_tuning_rules.TuningMetadata
	checksum string
	client   rpc_tuning_rules.TuningRuleClient
}

func newRpcTuningRule(meta *rpc_tuning_rules.TuningMetadata, client rpc_tuning_rules.TuningRuleClient, checksum string) *rpcTuningRule {
	return &rpcTuningRule{meta: meta, checksum: checksum, client: client}
}

func (r *rpcTuningRule) Id() string {
	if id := r.meta.GetId(); id != "" {
		return id
	}
	return r.meta.GetName()
}
func (r *rpcTuningRule) Name() string {
	return r.meta.GetName()
}

func (r *rpcTuningRule) Description() string {
	return r.meta.GetDescription()
}

func (r *rpcTuningRule) Enabled() bool {
	return r.meta.GetEnabled()
}

func (r *rpcTuningRule) Version() string {
	return r.meta.GetVersion()
}

func (r *rpcTuningRule) Checksum() string {
	return r.checksum
}

func (r *rpcTuningRule) String() string {
	return fmt.Sprintf("TuningRule '%s' (id:%s, enabled:%t)", r.meta.GetName(), r.meta.GetId(), r.meta.GetEnabled())
}

func (r *rpcTuningRule) Global() bool {
	return r.meta.GetGlobal()
}

func (r *rpcTuningRule) RuleType() RuleType {
	return RuleType(r.meta.GetRuleType())
}

func (r *rpcTuningRule) Confidence() scoring.Confidence {
	conf, _ := scoring.ParseConfidence(r.meta.GetConfidence())
	return conf
}

func (r *rpcTuningRule) Tune(ctx context.Context, alrts []alerts.Alert) ([]bool, errors.Error) {
	alertJSONs := make([][]byte, 0, len(alrts))
	for _, alrt := range alrts {
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
