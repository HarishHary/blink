package matchers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

type rpcMatcher struct {
	cfg      config.Source[*MatcherMetadata]
	fileName string
	checksum string
	client   rpc_matchers.MatcherClient
}

func newRpcMatcher(fileName string, client rpc_matchers.MatcherClient, cfg config.Source[*MatcherMetadata], checksum string) *rpcMatcher {
	return &rpcMatcher{
		cfg:      cfg,
		fileName: fileName,
		checksum: checksum,
		client:   client,
	}
}

func (r *rpcMatcher) config() *MatcherMetadata {
	if r.cfg == nil {
		return nil
	}
	v, _ := r.cfg.ByFileName(r.fileName)
	return v
}

// MatcherMetadata returns the live YAML-derived matcher configuration.
func (r *rpcMatcher) MatcherMetadata() *MatcherMetadata {
	if c := r.config(); c != nil {
		return c
	}
	return &MatcherMetadata{PluginMetadata: plugin.PluginMetadata{Id: r.fileName, Name: r.fileName}}
}

func (r *rpcMatcher) Metadata() plugin.PluginMetadata {
	if c := r.config(); c != nil {
		return c.Metadata()
	}
	return plugin.PluginMetadata{Id: r.fileName, Name: r.fileName}
}

func (r *rpcMatcher) Checksum() string { return r.checksum }
func (r *rpcMatcher) String() string {
	m := r.MatcherMetadata().Metadata()
	return fmt.Sprintf("Matcher '%s' (id:%s)", m.Name, m.Id)
}

func (r *rpcMatcher) Match(ctx context.Context, evts []events.Event) ([]bool, errors.Error) {
	protoEvents := make([]*rpc_matchers.Event, 0, len(evts))
	for _, ev := range evts {
		b, err := json.Marshal(ev)
		if err != nil {
			return nil, errors.NewE(err)
		}
		protoEvents = append(protoEvents, &rpc_matchers.Event{Json: b})
	}
	resp, err := r.client.MatchBatch(ctx, &rpc_matchers.MatchBatchRequest{Events: protoEvents})
	if err != nil {
		return nil, errors.NewE(err)
	}
	return resp.GetMatched(), nil
}
