// Each formatter binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/formatters-schema.md.

package formatters

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

// FormatterSnapshotConfig is the formatter instantiation of config.SnapshotConfig.
type FormatterSnapshotConfig = config.SnapshotConfig[*FormatterMetadata]

// FormatterLoader implements config.Loader[*FormatterMetadata] for formatters.
// Embed config.BaseLoader to inherit default Parse, ParseSpec, Validate, and CrossValidate.
type FormatterLoader struct {
	config.BaseLoader[FormatterMetadata, *FormatterMetadata]
}

// NewFormatterSnapshotConfig builds the snapshot-backed formatter config, parsing specs with
// FormatterLoader.
func NewFormatterSnapshotConfig(logger *logger.Logger, src plugin.SnapshotSource) *FormatterSnapshotConfig {
	return config.NewSnapshotConfig[*FormatterMetadata](logger, src, FormatterLoader{})
}
