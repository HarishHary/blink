// Each matcher binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/matchers-schema.md.

package matchers

import "github.com/harishhary/blink/internal/runtime/plugin"

// Loader implements pluginruntime.Loader[*MatcherMetadata] for matchers.
// Embed pluginruntime.BaseLoader to inherit default Parse, ParseSpec, Validate, and CrossValidate.
type Loader struct {
	plugin.BaseLoader[MatcherMetadata, *MatcherMetadata]
}
