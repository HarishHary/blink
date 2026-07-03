// Each matcher binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/matchers-schema.md.

package matchers

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

// MatcherSnapshotConfig is the control plane backed config source (published snapshot)
type MatcherSnapshotConfig = config.SnapshotConfig[*MatcherMetadata]

// Loader implements config.Loader[*MatcherMetadata] for matchers.
// Embed config.BaseLoader to inherit default Parse, ParseSpec, Validate, and CrossValidate.
type MatcherLoader struct {
	config.BaseLoader[MatcherMetadata, *MatcherMetadata]
}

// NewMatcherSnapshotConfig builds the snapshot-backed matcher config, parsing specs with
// MatcherLoader.
func NewMatcherSnapshotConfig(logger *logger.Logger, src plugin.SnapshotSource) *MatcherSnapshotConfig {
	return config.NewSnapshotConfig[*MatcherMetadata](logger, src, MatcherLoader{})
}
