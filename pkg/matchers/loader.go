// Each matcher binary ships alongside a <name>.yaml sidecar file.
//
// YAML schema example:
//
//	id: "550e8400-e29b-41d4-a716-446655440002"
//	name: "prod-accounts"
//	display_name: "Production Accounts Matcher"
//	description: "Matches events from production AWS accounts."
//	enabled: true
//	version: "1.0.0"
//	file_name: "prod-accounts"
//	global: false
//	mode: "blue-green"
//	min_procs: 1
//	max_procs: 2

package matchers

import (
	cfg "github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
)

// Registry and Manager are the generic implementations parameterised for matchers.
type MatcherConfigManager = cfg.ConfigManager[*MatcherMetadata]

// Loader implements cfg.Loader[*MatcherMetadata] for matchers.
// Embed cfg.BaseLoader to inherit default Parse, Validate, and CrossValidate.
type Loader struct {
	cfg.BaseLoader[MatcherMetadata, *MatcherMetadata]
}

func NewMatcherConfigManager(log *logger.Logger, dir string) *MatcherConfigManager {
	return cfg.NewConfigManager[*MatcherMetadata](log, "matcher", dir, Loader{})
}
