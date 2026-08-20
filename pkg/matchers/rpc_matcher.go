package matchers

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"

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

func (r *rpcMatcher) MatchBatch(ctx context.Context, events []evts.Event) MatchResult {
	protoEvents := make([]*structpb.Struct, 0, len(events))
	for _, event := range events {
		s, err := structpb.NewStruct(event)
		if err != nil {
			return MatchResult{CallErr: errors.NewE(err)}
		}
		protoEvents = append(protoEvents, s)
	}
	resp, err := r.client.MatchBatch(ctx, &rpc_matchers.MatchBatchRequest{Events: protoEvents})
	if err != nil {
		return MatchResult{CallErr: errors.NewE(err)}
	}
	if resp == nil {
		return MatchResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "matcher", PluginID: r.fileName, Field: "response", Expected: 1})}
	}
	if len(resp.GetItems()) != len(events) {
		return MatchResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "matcher", PluginID: r.fileName, Field: "items", Expected: len(events), Actual: len(resp.GetItems())})}
	}
	items := make([]MatchItem, len(events))
	for i, item := range resp.GetItems() {
		items[i].Matched = item.GetMatched()
		if item.GetError() != "" {
			items[i].Err = errors.New(item.GetError())
		}
	}
	return MatchResult{Items: items}
}
