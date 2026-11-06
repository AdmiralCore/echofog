package media

import (
	"bytes"
	"crypto/ed25519"
	"testing"
	"time"
)

func testMediaKey(b byte) (ed25519.PrivateKey, ed25519.PublicKey) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, 32))
	return priv, priv.Public().(ed25519.PublicKey)
}

func unit3(kind byte, seq uint32) (Header3, []byte) {
	h := Header3{Kind: kind, Epoch: 1, Sender: 3, Stream: 0, Gen: 1, Seq: seq, TS: seq * 160}
	return h, bytes.Repeat([]byte{byte(seq)}, 30)
}

func TestAuthRoundTripAndTamper(t *testing.T) {
	priv, pub := testMediaKey(1)
	a := &Auth{Kind: Kind3Audio, Epoch: 2, Sender: 5, Stream: 1, Gen: 3, FirstSeq: 100,
		Prev: [][UnitHash3Len]byte{{1}, {2}}, Cur: [][UnitHash3Len]byte{{3}, {4}, {5}}}
	payload := a.Sign(priv)
	if want := 5 + 4 + 1 + 1 + 5*UnitHash3Len + AuthSigLen3; len(payload) != want {
		t.Fatalf("payload length %d", len(payload))
	}
	got, err := DecodeAuth(payload)
	if err != nil || !got.Verify(pub) {
		t.Fatalf("decode/verify: %v", err)
	}
	if len(got.Prev) != 2 || len(got.Cur) != 3 || got.FirstSeq != 100 || got.Cur[2] != [UnitHash3Len]byte{5} {
		t.Fatalf("decoded %+v", got)
	}
	for i := range payload[:len(payload)-AuthSigLen3] {
		bad := append([]byte(nil), payload...)
		bad[i] ^= 1
		if d, err := DecodeAuth(bad); err == nil && d.Verify(pub) {
			t.Fatalf("flipping byte %d still verifies", i)
		}
	}
	_, other := testMediaKey(2)
	if got.Verify(other) {
		t.Fatal("verifies under another key")
	}
}

func TestDecodeAuthRejectsMalformed(t *testing.T) {
	priv, _ := testMediaKey(1)
	a := &Auth{Kind: 1, Sender: 1, Cur: [][UnitHash3Len]byte{{1}}}
	good := a.Sign(priv)
	prefix := 11 // kind epoch sender stream gen first_seq prev_count count
	cases := map[string][]byte{
		"empty":        nil,
		"truncated":    good[:len(good)-1],
		"trailing":     append(append([]byte(nil), good...), 0),
		"count zero":   func() []byte { b := append([]byte(nil), good...); b[prefix-1] = 0; return b }(),
		"count 17":     func() []byte { b := append([]byte(nil), good...); b[prefix-1] = 17; return b }(),
		"prev_count17": func() []byte { b := append([]byte(nil), good...); b[prefix-2] = 17; return b }(),
	}
	for name, b := range cases {
		if _, err := DecodeAuth(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func FuzzDecodeAuth(f *testing.F) {
	priv, _ := testMediaKey(1)
	a := &Auth{Kind: 1, Sender: 1, FirstSeq: 9, Prev: [][UnitHash3Len]byte{{1}}, Cur: [][UnitHash3Len]byte{{2}, {3}}}
	f.Add(a.Sign(priv))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := DecodeAuth(data)
		if err != nil {
			return
		}
		if out := append(d.Marshal(), d.Sig...); !bytes.Equal(out, data) {
			t.Fatalf("round trip changed the CALL_AUTH:\n in %x\nout %x", data, out)
		}
	})
}

// addUnits feeds n consecutive units into b and returns the CALL_AUTH payloads it produced and
// the units' hashes.
func addUnits(b *Batcher, kind byte, from uint32, n int) ([][]byte, [][UnitHash3Len]byte) {
	var auths [][]byte
	var hs [][UnitHash3Len]byte
	for i := 0; i < n; i++ {
		h, ct := unit3(kind, from+uint32(i))
		hs = append(hs, UnitHash3(h, ct))
		auths = append(auths, b.Add(h, ct)...)
	}
	return auths, hs
}

func TestBatchSizes(t *testing.T) {
	priv, pub := testMediaKey(1)
	for _, tc := range []struct {
		kind byte
		n    int
	}{{Kind3Audio, 5}, {Kind3ScreenAudio, 5}, {Kind3Video, 8}, {Kind3VideoFEC, 8}, {Kind3Heartbeat, 8}} {
		clk := newFakeClock()
		b := NewBatcher(priv, clk.Now)
		auths, hs := addUnits(b, tc.kind, 10, tc.n-1)
		if len(auths) != 0 {
			t.Fatalf("kind %d: batch closed after %d units", tc.kind, tc.n-1)
		}
		auths, hs2 := addUnits(b, tc.kind, 10+uint32(tc.n-1), 1)
		hs = append(hs, hs2...)
		if len(auths) != 1 {
			t.Fatalf("kind %d: %d CALL_AUTH after %d units", tc.kind, len(auths), tc.n)
		}
		a, err := DecodeAuth(auths[0])
		if err != nil || !a.Verify(pub) {
			t.Fatalf("kind %d: %v", tc.kind, err)
		}
		if a.FirstSeq != 10 || len(a.Prev) != 0 || len(a.Cur) != tc.n || a.Cur[tc.n-1] != hs[tc.n-1] {
			t.Fatalf("kind %d: %+v", tc.kind, a)
		}
	}
}

func TestBatchClosesAfter100ms(t *testing.T) {
	priv, pub := testMediaKey(1)
	clk := newFakeClock()
	b := NewBatcher(priv, clk.Now)
	addUnits(b, Kind3Audio, 1, 2)
	clk.Advance(BatchMaxAge - time.Millisecond)
	if got := b.Tick(); len(got) != 0 {
		t.Fatal("closed before 100 ms")
	}
	clk.Advance(time.Millisecond)
	got := b.Tick()
	if len(got) != 1 {
		t.Fatalf("%d CALL_AUTH at 100 ms", len(got))
	}
	if a, _ := DecodeAuth(got[0]); a == nil || !a.Verify(pub) || len(a.Cur) != 2 {
		t.Fatal("bad short batch")
	}
	if len(b.Tick()) != 0 {
		t.Fatal("empty batch produced a CALL_AUTH")
	}
	// a unit that arrives after the batch expired closes the old batch and opens a new one.
	addUnits(b, Kind3Audio, 3, 1)
	clk.Advance(BatchMaxAge)
	auths, _ := addUnits(b, Kind3Audio, 4, 1)
	if len(auths) != 1 {
		t.Fatalf("expired batch not closed by the next unit: %d", len(auths))
	}
	if a, _ := DecodeAuth(auths[0]); len(a.Cur) != 1 || a.FirstSeq != 3 {
		t.Fatalf("expired batch %+v", a)
	}
}

func TestEachCallAuthCoversPreviousBatch(t *testing.T) {
	priv, pub := testMediaKey(1)
	clk := newFakeClock()
	b := NewBatcher(priv, clk.Now)
	a1, h1 := addUnits(b, Kind3Audio, 0, 5)
	a2, h2 := addUnits(b, Kind3Audio, 5, 5)
	if len(a1) != 1 || len(a2) != 1 {
		t.Fatal("two batches expected")
	}
	d2, _ := DecodeAuth(a2[0])
	if !d2.Verify(pub) || d2.FirstSeq != 5 || len(d2.Prev) != 5 || len(d2.Cur) != 5 {
		t.Fatalf("second CALL_AUTH %+v", d2)
	}
	for i := range h1 {
		if d2.Prev[i] != h1[i] || d2.Cur[i] != h2[i] {
			t.Fatalf("hash %d differs", i)
		}
	}
	// a gap in the sequence: the previous batch is not adjacent, so it is not repeated.
	a3, _ := addUnits(b, Kind3Audio, 100, 5)
	d3, _ := DecodeAuth(a3[0])
	if len(d3.Prev) != 0 || d3.FirstSeq != 100 {
		t.Fatalf("non-adjacent batch repeats the previous: %+v", d3)
	}
}
