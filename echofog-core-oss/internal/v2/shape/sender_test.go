package shape

import (
	"encoding/binary"
	mrand "math/rand/v2"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func newRand(seed uint64) *mrand.Rand { return mrand.New(mrand.NewPCG(seed, seed+1)) }

func mustProfile(t *testing.T, p *Profile) *Profile {
	t.Helper()
	if err := p.Validate(); err != nil {
		t.Fatalf("profile: %v", err)
	}
	return p
}

func TestNextInitLen(t *testing.T) {
	s := NewSender(mustProfile(t, &Profile{InitLen: &Range{1280, 1420}, Keepalive: Range{12, 24}}), newRand(1), false)
	for i := 0; i < 1000; i++ {
		n := s.NextInitLen()
		if n < 1280 || n > 1420 {
			t.Fatalf("NextInitLen() = %d, want [1280, 1420]", n)
		}
	}
	fixed := NewSender(mustProfile(t, &Profile{InitLen: &Range{1300, 1300}, Keepalive: Range{12, 24}}), newRand(2), false)
	if n := fixed.NextInitLen(); n != 1300 {
		t.Fatalf("NextInitLen() = %d, want 1300", n)
	}
	none := NewSender(nil, newRand(3), false)
	if n := none.NextInitLen(); n != 0 {
		t.Fatalf("none NextInitLen() = %d, want 0", n)
	}
}

func TestPadForUniform(t *testing.T) {
	p := mustProfile(t, &Profile{Pad: &Range{10, 20}, Keepalive: Range{12, 24}})
	s := NewSender(p, newRand(4), false)
	for i := 0; i < 1000; i++ {
		if pad := s.PadFor(100); pad < 10 || pad > 20 {
			t.Fatalf("PadFor(100) = %d, want [10, 20]", pad)
		}
	}
	if pad := NewSender(nil, newRand(5), false).PadFor(100); pad != 0 {
		t.Fatalf("none PadFor(100) = %d, want 0", pad)
	}
	if pad := s.PadFor(-1); pad != 0 {
		t.Fatalf("PadFor(-1) = %d, want 0", pad)
	}
}

func TestPadForBuckets(t *testing.T) {
	p := mustProfile(t, &Profile{
		Pad:       &Range{0, 256},
		Buckets:   []uint32{512, 1024, 1460, 2920, 4096},
		PadBudget: &Range{1 << 20, 1 << 20},
		Keepalive: Range{12, 24},
	})
	cases := []struct{ n, want int }{
		{0, 512},
		{100, 412},
		{511, 1},
		{512, 0},
		{513, 511},
		{1024, 0},
		{1460, 0},
		{2920, 0},
		{4096, 0},
		{4097, 0},
	}
	for _, tc := range cases {
		s := NewSender(p, newRand(6), false)
		if pad := s.PadFor(tc.n); pad != tc.want {
			t.Errorf("PadFor(%d) = %d, want %d", tc.n, pad, tc.want)
		}
	}
}

func TestPadBudgetPerDirection(t *testing.T) {
	p := mustProfile(t, &Profile{
		Buckets:   []uint32{512, 1024},
		PadBudget: &Range{100, 100},
		Keepalive: Range{12, 24},
	})
	s := NewSender(p, newRand(7), false)
	// The first 100 bytes of the direction are inside the padding window: pad is capped by it.
	if pad := s.PadFor(100); pad != 100 {
		t.Fatalf("first PadFor(100) = %d, want 100", pad)
	}
	if pad := s.PadFor(100); pad != 0 {
		t.Fatalf("second PadFor(100) = %d, want 0 (budget spent)", pad)
	}

	// A zero-length window disables padding from the first byte.
	zero := NewSender(mustProfile(t, &Profile{Buckets: []uint32{512}, PadBudget: &Range{0, 0}, Keepalive: Range{12, 24}}), newRand(8), false)
	if pad := zero.PadFor(10); pad != 0 {
		t.Fatalf("zero-budget PadFor(10) = %d, want 0", pad)
	}

	// Without a budget, padding never stops.
	unbounded := NewSender(mustProfile(t, &Profile{Buckets: []uint32{512}, Keepalive: Range{12, 24}}), newRand(9), false)
	last := 0
	for i := 0; i < 100; i++ {
		last = unbounded.PadFor(100)
	}
	if last != 412 {
		t.Fatalf("unbounded PadFor(100) = %d after 100 calls, want 412", last)
	}
}

func TestJunk(t *testing.T) {
	p := mustProfile(t, &Profile{
		Junk:      &Junk{Count: Range{2, 2}, Len: Range{40, 40}, Gap: Range{7, 7}},
		Keepalive: Range{12, 24},
	})
	got := NewSender(p, newRand(10), false).Junk()
	if len(got) != 2 {
		t.Fatalf("Junk() returned %d datagrams, want 2", len(got))
	}
	for i, d := range got {
		if len(d.Data) != 40 {
			t.Errorf("junk %d: len %d, want 40", i, len(d.Data))
		}
		if d.Gap != 7*time.Millisecond {
			t.Errorf("junk %d: gap %v, want 7ms", i, d.Gap)
		}
	}

	// The default noise3 profile draws 0..2 junk datagrams of 32..256 bytes with 5..30 ms gaps.
	udp, err := Builtin(IDDefaultUDP)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSender(udp, newRand(11), false)
	seen := map[int]bool{}
	for i := 0; i < 200; i++ {
		j := s.Junk()
		if len(j) > 2 {
			t.Fatalf("Junk() returned %d datagrams, want [0, 2]", len(j))
		}
		seen[len(j)] = true
		for _, d := range j {
			if len(d.Data) < 32 || len(d.Data) > 256 {
				t.Fatalf("junk len %d, want [32, 256]", len(d.Data))
			}
			if d.Gap < 5*time.Millisecond || d.Gap > 30*time.Millisecond {
				t.Fatalf("junk gap %v, want [5, 30]ms", d.Gap)
			}
		}
	}
	if len(seen) < 2 {
		t.Fatalf("default-udp junk count never varied: %v", seen)
	}

	if j := NewSender(nil, newRand(12), false).Junk(); len(j) != 0 {
		t.Fatalf("none Junk() = %v, want none", j)
	}
}

func TestDecoysRawTemplate(t *testing.T) {
	tmpl := []byte{0xAA, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xBB}
	p := mustProfile(t, &Profile{Decoys: []Decoy{{Kind: DecoyRaw, Template: tmpl}}, Keepalive: Range{12, 24}})
	got := NewSender(p, newRand(13), false).Decoys()
	if len(got) != 1 {
		t.Fatalf("Decoys() returned %d datagrams, want 1", len(got))
	}
	d := got[0]
	if d.Gap != 0 {
		t.Errorf("decoy gap = %v, want 0", d.Gap)
	}
	if len(d.Data) != len(tmpl) {
		t.Fatalf("decoy len %d, want %d", len(d.Data), len(tmpl))
	}
	if d.Data[0] != 0xAA || d.Data[len(d.Data)-1] != 0xBB {
		t.Fatalf("template bytes outside the 0xff run changed: % x", d.Data)
	}
	all := true
	for _, b := range d.Data[1 : len(d.Data)-1] {
		if b != 0xff {
			all = false
		}
	}
	if all {
		t.Fatalf("0xff run was not replaced: % x", d.Data)
	}
}

func TestDecoysDNS(t *testing.T) {
	p := mustProfile(t, &Profile{
		Decoys:    []Decoy{{Kind: DecoyDNS, QName: "example.org."}},
		Keepalive: Range{12, 24},
	})
	got := NewSender(p, newRand(14), false).Decoys()
	if len(got) != 1 {
		t.Fatalf("Decoys() returned %d datagrams, want 1", len(got))
	}
	var m dnsmessage.Message
	if err := m.Unpack(got[0].Data); err != nil {
		t.Fatalf("decoy is not a DNS message: %v", err)
	}
	if len(m.Questions) != 1 {
		t.Fatalf("DNS decoy has %d questions, want 1", len(m.Questions))
	}
	q := m.Questions[0]
	if q.Name.String() != "example.org." {
		t.Errorf("qname = %q, want example.org.", q.Name.String())
	}
	if q.Type != dnsmessage.TypeA || q.Class != dnsmessage.ClassINET {
		t.Errorf("question type/class = %v/%v, want A/INET", q.Type, q.Class)
	}
}

func TestDecoysQUICInitial(t *testing.T) {
	p := mustProfile(t, &Profile{
		Decoys:    []Decoy{{Kind: DecoyQUICInitial}},
		Keepalive: Range{12, 24},
	})
	got := NewSender(p, newRand(15), false).Decoys()
	if len(got) != 1 {
		t.Fatalf("Decoys() returned %d datagrams, want 1", len(got))
	}
	d := got[0].Data
	if len(d) != 1200 {
		t.Fatalf("QUIC-shaped decoy is %d bytes, want 1200", len(d))
	}
	if d[0]&0xc0 != 0xc0 {
		t.Errorf("first byte %#02x is not a QUIC long header", d[0])
	}
	if v := binary.BigEndian.Uint32(d[1:5]); v != 1 {
		t.Errorf("QUIC version = %#08x, want 1", v)
	}
	if d[5] != 8 {
		t.Errorf("DCID length = %d, want 8", d[5])
	}
	if d[14] != 0 {
		t.Errorf("SCID length = %d, want 0", d[14])
	}
	if d[15] != 0 {
		t.Errorf("token length = %d, want 0", d[15])
	}
}

func TestKeepalive(t *testing.T) {
	udp, err := Builtin(IDDefaultUDP)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSender(udp, newRand(16), false)
	for i := 0; i < 1000; i++ {
		if d := s.Keepalive(); d < 12*time.Second || d > 24*time.Second {
			t.Fatalf("Keepalive() = %v, want [12, 24]s", d)
		}
	}
	custom := NewSender(mustProfile(t, &Profile{Keepalive: Range{30, 30}}), newRand(17), false)
	if d := custom.Keepalive(); d != 30*time.Second {
		t.Fatalf("Keepalive() = %v, want 30s", d)
	}
	// The zero value of Profile falls back to the wire.md §7.1 interval.
	zero := NewSender(&Profile{}, newRand(18), false)
	if d := zero.Keepalive(); d < 12*time.Second || d > 24*time.Second {
		t.Fatalf("zero-profile Keepalive() = %v, want [12, 24]s", d)
	}
}

func TestIdlePad(t *testing.T) {
	udp, err := Builtin(IDDefaultUDP)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSender(udp, newRand(19), false)
	for i := 0; i < 1000; i++ {
		d, ok := s.IdlePad()
		if !ok {
			t.Fatal("IdlePad() disabled on default-udp")
		}
		if d < 500*time.Millisecond || d > 4000*time.Millisecond {
			t.Fatalf("IdlePad() = %v, want [500, 4000]ms", d)
		}
	}
	if _, ok := NewSender(udp, newRand(20), true).IdlePad(); ok {
		t.Fatal("IdlePad() enabled under low_power")
	}
	none, err := Builtin(IDNone)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := NewSender(none, newRand(21), false).IdlePad(); ok {
		t.Fatal("IdlePad() enabled on a profile without idle_pad_ms")
	}
}

func TestNilRand(t *testing.T) {
	// A nil source uses the package-level generator; values must still respect the profile.
	s := NewSender(mustProfile(t, &Profile{InitLen: &Range{1300, 1300}, Keepalive: Range{30, 30}}), nil, false)
	if n := s.NextInitLen(); n != 1300 {
		t.Fatalf("NextInitLen() = %d, want 1300", n)
	}
	if pad := s.PadFor(10); pad != 0 {
		t.Fatalf("PadFor(10) = %d, want 0", pad)
	}
	if d := s.Keepalive(); d != 30*time.Second {
		t.Fatalf("Keepalive() = %v, want 30s", d)
	}
	if _, ok := s.IdlePad(); ok {
		t.Fatal("IdlePad() enabled without idle_pad_ms")
	}
	raw := NewSender(mustProfile(t, &Profile{Decoys: []Decoy{{Kind: DecoyRaw, Template: []byte{1, 2}}}, Keepalive: Range{12, 24}}), nil, false).Decoys()
	if len(raw) != 1 || len(raw[0].Data) != 2 || raw[0].Data[0] != 1 || raw[0].Data[1] != 2 {
		t.Fatalf("Decoys() = %+v, want the unchanged template", raw)
	}
}

func TestDeterministic(t *testing.T) {
	p := mustProfile(t, &Profile{
		InitLen:   &Range{1280, 1420},
		Pad:       &Range{0, 48},
		Junk:      &Junk{Count: Range{0, 2}, Len: Range{32, 64}, Gap: Range{1, 5}},
		Keepalive: Range{12, 24},
	})
	a := NewSender(p, newRand(22), false)
	b := NewSender(p, newRand(22), false)
	for i := 0; i < 20; i++ {
		if x, y := a.NextInitLen(), b.NextInitLen(); x != y {
			t.Fatalf("draw %d: init len %d != %d", i, x, y)
		}
		if x, y := a.PadFor(100), b.PadFor(100); x != y {
			t.Fatalf("draw %d: pad %d != %d", i, x, y)
		}
		if x, y := a.Keepalive(), b.Keepalive(); x != y {
			t.Fatalf("draw %d: keepalive %v != %v", i, x, y)
		}
		ja, jb := a.Junk(), b.Junk()
		if len(ja) != len(jb) {
			t.Fatalf("draw %d: junk count %d != %d", i, len(ja), len(jb))
		}
		for k := range ja {
			if ja[k].Gap != jb[k].Gap || string(ja[k].Data) != string(jb[k].Data) {
				t.Fatalf("draw %d, junk %d differs", i, k)
			}
		}
	}
	da := NewSender(mustProfile(t, &Profile{Decoys: []Decoy{{Kind: DecoyRaw, Template: []byte{0xff, 0xff, 0xff}}}, Keepalive: Range{12, 24}}), newRand(23), false).Decoys()
	db := NewSender(mustProfile(t, &Profile{Decoys: []Decoy{{Kind: DecoyRaw, Template: []byte{0xff, 0xff, 0xff}}}, Keepalive: Range{12, 24}}), newRand(23), false).Decoys()
	if len(da) != 1 || len(db) != 1 || string(da[0].Data) != string(db[0].Data) {
		t.Fatalf("decoy generation is not deterministic: % x / % x", da, db)
	}
}
