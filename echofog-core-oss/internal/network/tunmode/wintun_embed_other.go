//go:build windows && !amd64 && !arm64

package tunmode

// wintunDLL is empty on Windows architectures we do not ship the driver for; ensureWintun then
// falls back to requiring the file to be present beside the executable.
var wintunDLL []byte
