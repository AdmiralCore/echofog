//go:build windows && amd64

package tunmode

import _ "embed"

// wintunDLL is the Wintun driver (the signed amd64 build from wintun.net, by WireGuard LLC),
// embedded so the Windows build needs no DLL shipped beside it. It is written to disk on demand
// before the TUN adapter is created; see ensureWintun.
//
//go:embed wintun_amd64.bin
var wintunDLL []byte
