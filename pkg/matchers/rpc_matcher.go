package matchers

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
	evts "github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

type rpcMatcher struct {
	metadata MatcherMetadata
	fileName string
	checksum string
	client   rpc_matchers.MatcherClient
}

func newRpcMatcher(fileName string, client rpc_matchers.MatcherClient, metadata MatcherMetadata, checksum string) *rpcMatcher {
	return &rpcMatcher{
		metadata: metadata,
		fileName: fileName,
		checksum: checksum,
		client:   client,
	}
}

// MatcherMetadata returns an independently owned snapshot-derived matcher configuration.
func (r *rpcMatcher) MatcherMetadata() *MatcherMetadata {
	return r.metadata.Clone()
}

func (r *rpcMatcher) Metadata() plugin.Spec {
	return r.MatcherMetadata().Metadata()
}

func (r *rpcMatcher) Checksum() string { return r.checksum }

func (r *rpcMatcher) MatchBatch(ctx context.Context, batch *evts.Batch) MatchResult {
	resp, err := r.client.MatchBatch(ctx, &rpc_matchers.MatchBatchRequest{Events: batch.Raw()})
	if err != nil {
		return MatchResult{CallErr: errors.NewE(err)}
	}
	if resp == nil {
		return MatchResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "matcher", PluginID: r.fileName, Field: "response", Expected: 1})}
	}
	if len(resp.GetItems()) != batch.Len() {
		return MatchResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "matcher", PluginID: r.fileName, Field: "items", Expected: batch.Len(), Actual: len(resp.GetItems())})}
	}
	items := make([]MatchItem, batch.Len())
	for i, item := range resp.GetItems() {
		items[i].Matched = item.GetMatched()
		if item.GetError() != "" {
			items[i].Err = errors.New(item.GetError())
		}
	}
	return MatchResult{Items: items}
}
