// Package data embeds the default grounding seed so the binary works
// without extra files; GROUNDING_SEED_PATH overrides it at runtime.
package data

import _ "embed"

//go:embed ghana_grounding_seed.yaml
var GroundingSeed []byte
