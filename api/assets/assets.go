// Package assets holds files baked into every agent image.
package assets

import _ "embed"

// Harness is the agent runtime copied to /trefoil/harness/main.py.
//
//go:embed trefoil-harness/main.py
var Harness []byte

// Skills is the harness's Agent Skills module, copied next to main.py; at
// build time it also installs skills from URLs.
//
//go:embed trefoil-harness/skills.py
var Skills []byte
