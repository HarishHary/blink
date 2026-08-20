// Each formatter binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/formatters-schema.md.

package formatters

import "github.com/harishhary/blink/internal/runtime/plugin"

// Loader implements plugin.Loader[*FormatterMetadata] for formatters.
// Embed plugin.BaseLoader to inherit default Parse, ParseSpec, Validate, and CrossValidate.
type Loader struct {
	plugin.BaseLoader[FormatterMetadata, *FormatterMetadata]
}
