//go:build windows && arm64

package tunmode

import _ "embed"

// wintunDLL is the Wintun driver (the signed arm64 build from wintun.net, by WireGuard LLC).
//
//go:embed wintun_arm64.bin
var wintunDLL []byte
