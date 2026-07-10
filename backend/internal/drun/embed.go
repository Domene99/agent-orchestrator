//go:build !bundled_drun

package drun

// embeddedDrunMCP is nil when ao is built without the bundled_drun tag.
// In that case Server.Start falls back to PATH / AO_DRUN_BIN discovery.
var embeddedDrunMCP []byte
