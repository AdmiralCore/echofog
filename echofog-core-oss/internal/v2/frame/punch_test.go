package frame

import (
	"errors"
	"net/netip"
	"testing"
)

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func TestPunchOfferRoundTrip(t *testing.T) {
	o := PunchOffer{Addrs: []netip.AddrPort{ap("203.0.113.5:4433"), ap("[2001:db8::1]:9")}, Nonce: 0x0102030405060708}
	b := o.Append(nil)
	got, err := ParsePunchOffer(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nonce != o.Nonce || len(got.Addrs) != 2 || got.Addrs[0] != o.Addrs[0] || got.Addrs[1] != o.Addrs[1] {
		t.Fatalf("got %+v", got)
	}
	if len(b) > 256 {
		t.Fatalf("offer is %d bytes, max 256", len(b))
	}
}

func TestPunchOfferMalformed(t *testing.T) {
	good := PunchOffer{Addrs: []netip.AddrPort{ap("203.0.113.5:4433")}, Nonce: 7}.Append(nil)
	cases := map[string][]byte{
		"truncated":       append(append([]byte{}, good...), 0x12)[:len(good)+1],
		"bad family":      {0x0a, 7, 9, 1, 2, 3, 4, 0, 5, 0x10, 7},
		"short addr":      {0x0a, 3, 4, 1, 2},
		"nonce wire type": {0x12, 1, 7},
		"unordered":       append([]byte{0x10, 7}, good...),
	}
	for name, b := range cases {
		if _, err := ParsePunchOffer(b); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

// More than MaxPunchAddrs addresses are malformed: a sender never sends them, and a parser that
// accepted any number would let one frame carry 256 bytes of garbage targets.
func TestPunchOfferTooManyAddrs(t *testing.T) {
	one := PunchOffer{Addrs: []netip.AddrPort{ap("203.0.113.5:4433")}}.Append(nil)
	var b []byte
	for i := 0; i < MaxPunchAddrs+1; i++ {
		b = append(b, one...)
	}
	if _, err := ParsePunchOffer(b); !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v", err)
	}
}

func FuzzParsePunchOffer(f *testing.F) {
	f.Add(PunchOffer{Addrs: []netip.AddrPort{ap("203.0.113.5:4433")}, Nonce: 9}.Append(nil))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		o, err := ParsePunchOffer(b)
		if err != nil {
			return
		}
		again, err := ParsePunchOffer(o.Append(nil))
		if err != nil || again.Nonce != o.Nonce || len(again.Addrs) != len(o.Addrs) {
			t.Fatalf("round trip: %v %+v %+v", err, o, again)
		}
	})
}

// The birthday-punch fields are additive (services.md §6.4.1): they round-trip, an offer without
// them parses to zeros, and an older parser (which skips unknown fields) still reads the base.
func TestPunchOfferBirthdayFields(t *testing.T) {
	o := PunchOffer{Addrs: []netip.AddrPort{ap("203.0.113.5:4433")}, Nonce: 9, Nat: 2, PortLo: 40000, PortHi: 41999, Samples: 5, Mode: 1}
	b := o.Append(nil)
	got, err := ParsePunchOffer(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nat != 2 || got.PortLo != 40000 || got.PortHi != 41999 || got.Samples != 5 || got.Mode != 1 || got.Nonce != 9 {
		t.Fatalf("got %+v", got)
	}
	if len(b) > 256 {
		t.Fatalf("offer is %d bytes, max 256", len(b))
	}
	plain := PunchOffer{Addrs: o.Addrs, Nonce: 9}.Append(nil)
	if len(plain) >= len(b) {
		t.Fatal("an offer without the fields must be shorter (fields are omitted when zero)")
	}
	g2, err := ParsePunchOffer(plain)
	if err != nil || g2.Nat != 0 || g2.Mode != 0 {
		t.Fatalf("plain offer parsed to %+v, %v", g2, err)
	}
}
