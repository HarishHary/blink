package matchers

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	evts "github.com/harishhary/blink/pkg/events"
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
	if len(resp.GetResults()) != len(events) {
		return MatchResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "matcher", PluginID: r.fileName, Field: "results", Expected: len(events), Actual: len(resp.GetResults())})}
	}
	if len(resp.GetErrors()) != len(events) {
		return MatchResult{CallErr: errors.NewE(&errors.ResultCardinalityError{PluginKind: "matcher", PluginID: r.fileName, Field: "errors", Expected: len(events), Actual: len(resp.GetErrors())})}
	}
	perErrs := make([]errors.Error, len(events))
	for i, message := range resp.GetErrors() {
		if message != "" {
			perErrs[i] = errors.New(message)
		}
	}
	return MatchResult{Results: resp.GetResults(), Errs: perErrs}
}
