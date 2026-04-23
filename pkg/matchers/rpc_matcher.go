package matchers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

type rpcMatcher struct {
	cfgManager *MatcherConfigManager
	fileName   string
	checksum   string
	client     rpc_matchers.MatcherClient
	timeout    time.Duration
}

func newRpcMatcher(fileName string, client rpc_matchers.MatcherClient, manager *MatcherConfigManager, timeout time.Duration, checksum string) *rpcMatcher {
	return &rpcMatcher{
		cfgManager: manager,
		fileName:   fileName,
		checksum:   checksum,
		client:     client,
		timeout:    timeout,
	}
}

func (r *rpcMatcher) cfg() *MatcherMetadata {
	if r.cfgManager == nil {
		return nil
	}
	v, _ := r.cfgManager.Current().ByFileName(r.fileName)
	return v
}

// MatcherMetadata returns the live YAML-derived matcher configuration.
func (r *rpcMatcher) MatcherMetadata() *MatcherMetadata {
	if c := r.cfg(); c != nil {
		return c
	}
	return &MatcherMetadata{PluginMetadata: plugin.PluginMetadata{Id: r.fileName, Name: r.fileName}}
}

func (r *rpcMatcher) Metadata() plugin.PluginMetadata {
	return r.MatcherMetadata().Metadata()
}

func (r *rpcMatcher) Global() bool     { return r.MatcherMetadata().Global }
func (r *rpcMatcher) Checksum() string { return r.checksum }
func (r *rpcMatcher) String() string {
	m := r.MatcherMetadata().Metadata()
	return "RpcMatcher '" + m.Name + "' id:'" + m.Id + "'"
}

func (r *rpcMatcher) Match(ctx context.Context, evts []events.Event) ([]bool, errors.Error) {
	protoEvents := make([]*rpc_matchers.Event, 0, len(evts))
	for _, ev := range evts {
		b, err := json.Marshal(ev)
		if err != nil {
			return nil, errors.New(err)
		}
		protoEvents = append(protoEvents, &rpc_matchers.Event{Json: b})
	}
	resp, err := r.client.MatchBatch(ctx, &rpc_matchers.MatchBatchRequest{Events: protoEvents})
	if err != nil {
		return nil, errors.New(err)
	}
	return resp.GetMatched(), nil
}
