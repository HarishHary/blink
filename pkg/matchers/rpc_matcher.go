package matchers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/config"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

type rpcMatcher struct {
	cfgWatcher *config.Watcher
	fileName   string
	checksum   string
	client     rpc_matchers.MatcherClient
	timeout    time.Duration
}

func newRpcMatcher(fileName string, client rpc_matchers.MatcherClient, watcher *config.Watcher, timeout time.Duration, checksum string) *rpcMatcher {
	return &rpcMatcher{
		cfgWatcher: watcher,
		fileName:   fileName,
		checksum:   checksum,
		client:     client,
		timeout:    timeout,
	}
}

func (r *rpcMatcher) cfg() *config.MatcherMetadata {
	if r.cfgWatcher == nil {
		return nil
	}
	return r.cfgWatcher.Current().ByFileName(r.fileName)
}

// MatcherMetadata returns the live YAML-derived matcher configuration.
func (r *rpcMatcher) MatcherMetadata() *config.MatcherMetadata {
	if c := r.cfg(); c != nil {
		return c
	}
	return &config.MatcherMetadata{FileNameField: r.fileName}
}

func (r *rpcMatcher) PluginMetadata() plugin.PluginMetadata {
	c := r.MatcherMetadata()
	return plugin.PluginMetadata{
		ID:          c.Id(),
		Name:        c.Name(),
		Description: c.Description(),
		Enabled:     c.Enabled(),
		Version:     c.Version(),
	}
}

func (r *rpcMatcher) Global() bool     { return r.MatcherMetadata().Global() }
func (r *rpcMatcher) Checksum() string { return r.checksum }
func (r *rpcMatcher) String() string {
	return "RpcMatcher '" + r.MatcherMetadata().Name() + "' id:'" + r.MatcherMetadata().Id() + "'"
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
