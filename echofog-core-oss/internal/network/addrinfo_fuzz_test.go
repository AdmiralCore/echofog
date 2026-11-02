package network

import (
	"testing"
)

// FuzzParseAddrInfo: node addresses come from links, peer exchange and saved settings. Parsing
// must not panic, and a named address must keep its peer id and carry no query or id in Addr.
func FuzzParseAddrInfo(f *testing.F) {
	id := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	f.Add("203.0.113.5:443/" + id + "?sni=example.com&srv=1")
	f.Add(RelayLinkPrefix + "[::1]:4242/" + id)
	f.Add("wss://example.com/ws/" + id)
	f.Add("ws://:/" + id)
	f.Add("?")
	f.Fuzz(func(t *testing.T, s string) {
		info := ParseAddrInfo(s)
		if info.Named {
			again := ParseAddrInfo(info.Addr + "/" + info.ID.String())
			if !again.Named || again.ID != info.ID || again.Addr != info.Addr {
				t.Fatalf("named address %q does not read back: %+v -> %+v", s, info, again)
			}
		}
	})
}
