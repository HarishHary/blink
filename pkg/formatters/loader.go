// Each formatter binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/formatters-schema.md.

package formatters

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

// SnapshotConfig is the formatter instantiation of config.SnapshotConfig.
type SnapshotConfig = config.SnapshotConfig[*FormatterMetadata]

// Loader implements config.Loader[*FormatterMetadata] for formatters.
// Embed config.BaseLoader to inherit default Parse, ParseSpec, Validate, and CrossValidate.
type Loader struct {
	config.BaseLoader[FormatterMetadata, *FormatterMetadata]
}

// NewSnapshotConfig builds the snapshot-backed formatter config, parsing specs with
// Loader.
func NewSnapshotConfig(logger *logger.Logger, src plugin.SnapshotSource) *SnapshotConfig {
	return config.NewSnapshotConfig[*FormatterMetadata](logger, src, Loader{})
}
