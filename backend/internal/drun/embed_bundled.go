//go:build bundled_drun

package drun

import _ "embed"

// embeddedDrunMCP contains the drun-mcp binary compiled for the target platform.
// It is stamped in by scripts/daemon-build.sh before go build is invoked.
//
//go:embed binaries/drun-mcp
var embeddedDrunMCP []byte
