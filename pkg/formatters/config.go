// Each formatter binary ships alongside a <name>.yaml sidecar file.
//
// YAML schema example:
//
//	id: "550e8400-e29b-41d4-a716-446655440001"
//	name: "json-summary"
//	display_name: "JSON Summary Formatter"
//	description: "Formats alert data as a structured JSON summary."
//	enabled: true
//	version: "1.0.0"
//	file_name: "json-summary"
//	mode: "blue-green"
//	min_procs: 1
//	max_procs: 2

package formatters

import (
	cfg "github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
)

// Registry and Manager are the generic implementations parameterised for formatters.
type Registry = cfg.Registry[*FormatterMetadata]
type FormatterConfigManager = cfg.ConfigManager[*FormatterMetadata]

// Loader implements cfg.Loader[*FormatterMetadata] for formatters.
// Embed cfg.BaseLoader to inherit default Parse, Validate, and CrossValidate.
type Loader struct {
	cfg.BaseLoader[FormatterMetadata, *FormatterMetadata]
}

func NewFormatterConfigManager(log *logger.Logger, dir string) *FormatterConfigManager {
	return cfg.NewConfigManager[*FormatterMetadata](log, "formatter", dir, Loader{})
}
