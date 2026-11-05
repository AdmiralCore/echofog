package exit

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// EXIT_UDP fragment 0 from docs/protocol/v2/vectors/frames.json. addr_len counts the whole
// address part, including its own byte: 0x0e for the 13-byte "192.0.2.53:53".
func TestFragmentVector(t *testing.T) {
	t.Parallel()

	raw := mustHex(t, "05000100010e3139322e302e322e35333a3533abcd")
	f, err := ParseFragment(raw)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	want := Fragment{Session: 5, Packet: 1, Index: 0, Count: 1, Addr: "192.0.2.53:53",
		Data: []byte{0xab, 0xcd}}
	if f.Session != want.Session || f.Packet != want.Packet || f.Index != want.Index ||
		f.Count != want.Count || f.Addr != want.Addr || !bytes.Equal(f.Data, want.Data) {
		t.Fatalf("fragment = %+v, want %+v", f, want)
	}
	if got := f.Append(nil); !bytes.Equal(got, raw) {
		t.Fatalf("Append = %x, want %x", got, raw)
	}

	// Only fragment 0 carries the address.
	mid := Fragment{Session: 5, Packet: 1, Index: 1, Count: 2, Data: []byte{0x01}}
	if got, err := ParseFragment(mid.Append(nil)); err != nil || got.Addr != "" || got.Index != 1 {
		t.Fatalf("mid fragment = %+v, %v", got, err)
	}
}

func TestFragmentRejectsMalformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"no session", []byte{}},
		{"short header", mustHex(t, "0500")},
		{"count zero", mustHex(t, "050001000000")},
		{"count nine", mustHex(t, "0500010009")},
		{"index equals count", mustHex(t, "0500010101")},
		{"fragment 0 without address", mustHex(t, "0500010001")},
		{"address truncated", mustHex(t, "0500010001ff61")},
		{"address not utf8", mustHex(t, "050001000101c3")},
		{"non-minimal session", mustHex(t, "80000001000100")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseFragment(tc.in); err == nil {
				t.Fatalf("%x: want an error", tc.in)
			} else if !errors.Is(err, frame.ErrMalformed) {
				t.Fatalf("%x: %v is not ErrMalformed", tc.in, err)
			}
		})
	}
}

func TestReassembler(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	clk := func() time.Time { return now }
	r := newReassembler(clk)

	// Out of order.
	if _, _, ok := r.feed(Fragment{Session: 1, Packet: 7, Index: 1, Count: 3, Data: []byte("bc")}); ok {
		t.Fatal("completed too early")
	}
	if _, _, ok := r.feed(Fragment{Session: 1, Packet: 7, Index: 2, Count: 3, Data: []byte("d")}); ok {
		t.Fatal("completed too early")
	}
	addr, data, ok := r.feed(Fragment{Session: 1, Packet: 7, Index: 0, Count: 3, Addr: "192.0.2.1:53", Data: []byte("a")})
	if !ok || addr != "192.0.2.1:53" || string(data) != "abcd" {
		t.Fatalf("complete = %q %q %v", addr, data, ok)
	}
	if r.pendingCount() != 0 {
		t.Fatalf("pending = %d after completion", r.pendingCount())
	}

	// Duplicates do not double-count.
	r.feed(Fragment{Session: 1, Packet: 8, Index: 0, Count: 2, Addr: "a:1", Data: []byte("x")})
	r.feed(Fragment{Session: 1, Packet: 8, Index: 0, Count: 2, Addr: "a:1", Data: []byte("x")})
	if _, _, ok := r.feed(Fragment{Session: 1, Packet: 8, Index: 1, Count: 2, Data: []byte("y")}); !ok {
		t.Fatal("duplicate blocked completion")
	}

	// A packet expires after FragmentTTL: the old fragment 0 is dropped, so fragment 1 alone
	// cannot complete a packet.
	now = now.Add(time.Second)
	r.feed(Fragment{Session: 1, Packet: 9, Index: 0, Count: 2, Addr: "a:1", Data: []byte("x")})
	now = now.Add(FragmentTTL + time.Millisecond)
	if _, _, ok := r.feed(Fragment{Session: 1, Packet: 9, Index: 1, Count: 2, Data: []byte("y")}); ok {
		t.Fatal("expired packet completed")
	}
	if r.pendingCount() != 1 {
		t.Fatalf("pending = %d, want the new partial packet", r.pendingCount())
	}
	now = now.Add(FragmentTTL + time.Millisecond)
	r.expire()
	if r.pendingCount() != 0 {
		t.Fatalf("pending = %d after expiry", r.pendingCount())
	}
}

func TestReassemblerBounds(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	r := newReassembler(func() time.Time { return now })
	for i := 0; i < MaxIncomplete; i++ {
		r.feed(Fragment{Session: 1, Packet: uint16(i), Index: 0, Count: 2, Addr: "a:1", Data: []byte("x")})
	}
	if r.pendingCount() != MaxIncomplete {
		t.Fatalf("pending = %d, want %d", r.pendingCount(), MaxIncomplete)
	}
	// The 65th incomplete packet is dropped, not admitted.
	r.feed(Fragment{Session: 1, Packet: 1000, Index: 0, Count: 2, Addr: "a:1", Data: []byte("x")})
	if r.pendingCount() != MaxIncomplete {
		t.Fatalf("pending = %d after overflow, want %d", r.pendingCount(), MaxIncomplete)
	}
	// Its second fragment cannot complete it either.
	if _, _, ok := r.feed(Fragment{Session: 1, Packet: 1000, Index: 1, Count: 2, Data: []byte("y")}); ok {
		t.Fatal("overflowing packet completed")
	}
}
