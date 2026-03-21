package matchers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

type rpcMatcher struct {
	client   rpc_matchers.MatcherClient
	meta     *rpc_matchers.MatcherMetadata
	checksum string
	timeout  time.Duration
}

func newRpcMatcher(meta *rpc_matchers.MatcherMetadata, client rpc_matchers.MatcherClient, timeout time.Duration, checksum string) *rpcMatcher {
	return &rpcMatcher{
		meta:     meta,
		checksum: checksum,
		client:   client,
		timeout:  timeout,
	}
}

func (r *rpcMatcher) Id() string {
	if id := r.meta.GetId(); id != "" {
		return id
	}
	return r.meta.GetName()
}
func (r *rpcMatcher) Name() string        { return r.meta.GetName() }
func (r *rpcMatcher) Description() string { return r.meta.GetDescription() }
func (r *rpcMatcher) Enabled() bool       { return r.meta.GetEnabled() }
func (r *rpcMatcher) Checksum() string    { return r.checksum }
func (r *rpcMatcher) String() string {
	return "RpcMatcher '" + r.meta.GetName() + "' id:'" + r.meta.GetId() + "'"
}

func (r *rpcMatcher) Match(ctx context.Context, event events.Event) (bool, errors.Error) {
	b, err := json.Marshal(event)
	if err != nil {
		return false, errors.New(err)
	}
	resp, err := r.client.Match(ctx, &rpc_matchers.MatchRequest{Event: &rpc_matchers.Event{Json: b}})
	if err != nil {
		return false, errors.New(err)
	}
	return resp.GetMatched(), nil
}
