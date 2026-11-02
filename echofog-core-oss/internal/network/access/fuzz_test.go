package access

import (
	"bytes"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// FuzzParseLink: an invitation link comes from anywhere (a chat, a web page, a QR code). Reading it
// must never panic, and a link that reads must read the same after it is written again.
func FuzzParseLink(f *testing.F) {
	var id types.PeerID
	id[0], id[31] = 1, 2
	secret := bytes.Repeat([]byte{7}, SecretLen)
	f.Add(FormatLink(Invite{Network: "echofog", Protocol: 102, Peer: id, Secret: secret, Addrs: []string{"203.0.113.5:443"}}))
	f.Add(FormatLink(Invite{Network: "echofog", Peer: id, Relays: []string{"203.0.113.9:443/" + id.String()}, SNI: "example.com", Server: true}))
	f.Add(WebLink(FormatLink(Invite{Network: "echofog", Peer: id, Addrs: []string{"wss://example.com/ws"}})))
	f.Add("echofog://join/?v=1&net=echofog&id=" + id.String() + "&a=1.2.3.4:5")
	f.Add("echofog://join?v=2")
	f.Add("https://x/#join?v=1")
	f.Add("ECHOFOG://JOIN?v=1&net=echofog")
	f.Add("echofog://join?%zz")
	f.Fuzz(func(t *testing.T, s string) {
		inv, err := ParseLink(s, "echofog")
		if err != nil {
			return
		}
		again, err := ParseLink(FormatLink(inv), "echofog")
		if err != nil {
			t.Fatalf("a link that was read does not read after writing it again: %v (%q)", err, FormatLink(inv))
		}
		if again.Peer != inv.Peer || again.Protocol != inv.Protocol || !bytes.Equal(again.Secret, inv.Secret) ||
			again.SNI != inv.SNI || again.Server != inv.Server || len(again.Addrs) != len(inv.Addrs) || len(again.Relays) != len(inv.Relays) {
			t.Fatalf("round trip changed the invite: %+v -> %+v", inv, again)
		}
		for i := range inv.Addrs {
			if again.Addrs[i] != inv.Addrs[i] {
				t.Fatalf("address %d changed: %q -> %q", i, inv.Addrs[i], again.Addrs[i])
			}
		}
	})
}

// FuzzFromWebLink: the https form of a link turns back into the same echofog:// link, and text
// that is no web link comes back as it is.
func FuzzFromWebLink(f *testing.F) {
	f.Add("https://admiralcore.github.io/echofog/join/#join?v=1")
	f.Add("https://x/#echofog://join/?v=1")
	f.Add("echofog://join/?a=b")
	f.Add("http://#")
	f.Add("hello")
	f.Fuzz(func(t *testing.T, s string) {
		out := FromWebLink(s)
		if w := WebLink(out); w != "" {
			if back := FromWebLink(w); back != dropJoinSlash(out) && back != out {
				t.Fatalf("web link of %q does not come back: %q", out, back)
			}
		}
		_ = IsLink(s)
	})
}
