// Each matcher binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/matchers-schema.md.

package matchers

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

// SnapshotConfig is the control plane backed config source (published snapshot)
type SnapshotConfig = config.SnapshotConfig[*MatcherMetadata]

// Loader implements config.Loader[*MatcherMetadata] for matchers.
// Embed config.BaseLoader to inherit default Parse, ParseSpec, Validate, and CrossValidate.
type Loader struct {
	config.BaseLoader[MatcherMetadata, *MatcherMetadata]
}

// NewSnapshotConfig builds the snapshot-backed matcher config, parsing specs with
// Loader.
func NewSnapshotConfig(logger *logger.Logger, src plugin.SnapshotSource) *SnapshotConfig {
	return config.NewSnapshotConfig[*MatcherMetadata](logger, src, Loader{})
}
