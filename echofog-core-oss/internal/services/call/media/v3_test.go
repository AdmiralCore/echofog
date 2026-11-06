package media

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeClock is the injected clock of the v3 tests (AD13).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type call2Vector struct {
	RoomSecret   string `json:"room_secret"`
	Epoch0Secret string `json:"epoch0_secret"`
	DeviceID     string `json:"sender_device_id"`
	JoinSalt     string `json:"join_salt"`
	KSender      string `json:"k_sender"`
	Units        []struct {
		Header string `json:"header"`
		Seq    uint32 `json:"seq"`
		Nonce  string `json:"nonce"`
		CT     string `json:"ciphertext_and_tag"`
		H      string `json:"h"`
	} `json:"units"`
	MediaKeyPub string `json:"media_key_pub"`
	CallAuth    struct {
		SignedPrefix string `json:"signed_prefix"`
		Sig          string `json:"sig"`
		Datagram     string `json:"datagram"`
	} `json:"call_auth"`
}

func loadCall2(t testing.TB) (call2Vector, ed25519.PrivateKey) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "v2", "testdata", "kdf.json"))
	if err != nil {
		t.Fatalf("read kdf.json: %v", err)
	}
	var f struct {
		Seeds map[string]string `json:"seeds"`
		Call2 call2Vector       `json:"call2"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	seed := unhex(t, f.Seeds["call/media_key"])
	return f.Call2, ed25519.NewKeyFromSeed(seed)
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pattern(n int, base byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = base + byte(i)
	}
	return b
}

func TestCall2Vector(t *testing.T) {
	v, priv := loadCall2(t)
	if got := Epoch0Secret(unhex(t, v.RoomSecret)); !bytes.Equal(got, unhex(t, v.Epoch0Secret)) {
		t.Fatalf("epoch0 secret %x", got)
	}
	var device [32]byte
	copy(device[:], unhex(t, v.DeviceID))
	key, err := SenderKey3(unhex(t, v.Epoch0Secret), device, unhex(t, v.JoinSalt))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, unhex(t, v.KSender)) {
		t.Fatalf("k_sender %x", key)
	}
	var hashes [][UnitHash3Len]byte
	for i, u := range v.Units {
		h, err := ParseHeader3(unhex(t, u.Header))
		if err != nil {
			t.Fatal(err)
		}
		if h.Seq != u.Seq || !bytes.Equal(h.Marshal(), unhex(t, u.Header)) {
			t.Fatalf("unit %d header %+v", i, h)
		}
		if n := Nonce3(h); !bytes.Equal(n[:], unhex(t, u.Nonce)) {
			t.Fatalf("unit %d nonce %x", i, n)
		}
		ct, err := Seal3(h, key, pattern(40, byte(0xc0+i)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ct, unhex(t, u.CT)) {
			t.Fatalf("unit %d ciphertext %x", i, ct)
		}
		hh := UnitHash3(h, ct)
		if !bytes.Equal(hh[:], unhex(t, u.H)) {
			t.Fatalf("unit %d hash %x", i, hh)
		}
		hashes = append(hashes, hh)
	}
	first, _ := ParseHeader3(unhex(t, v.Units[0].Header))
	a := &Auth{Kind: first.Kind, Epoch: first.Epoch, Sender: first.Sender, Stream: first.Stream,
		Gen: first.Gen, FirstSeq: first.Seq, Cur: hashes}
	payload := a.Sign(priv)
	if want := unhex(t, v.CallAuth.Datagram)[1:]; !bytes.Equal(payload, want) {
		t.Fatalf("CALL_AUTH payload\n got %x\nwant %x", payload, want)
	}
	if !bytes.Equal(a.Marshal(), unhex(t, v.CallAuth.SignedPrefix)) {
		t.Fatalf("signed prefix %x", a.Marshal())
	}
	dec, err := DecodeAuth(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !dec.Verify(unhex(t, v.MediaKeyPub)) {
		t.Fatal("vector CALL_AUTH does not verify under the vector media key")
	}
}

func TestHeaderAndFragmentRoundTrip(t *testing.T) {
	h := Header3{Kind: Kind3Video, Epoch: 9, Sender: 3, Stream: 2, Gen: 200, Seq: 0xdeadbeef, TS: 77}
	got, err := ParseHeader3(h.Marshal())
	if err != nil || got != h {
		t.Fatalf("header %+v err %v", got, err)
	}
	f := Fragment3{Header: h, Idx: 1, Count: 3, Payload: []byte("abc")}
	enc := f.Encode()
	if len(enc) != Header3Len+2+3 {
		t.Fatalf("len %d", len(enc))
	}
	back, err := DecodeFragment3(enc)
	if err != nil || back.Header != h || back.Idx != 1 || back.Count != 3 || string(back.Payload) != "abc" {
		t.Fatalf("fragment %+v err %v", back, err)
	}
}

func TestDecodeFragmentRejectsMalformed(t *testing.T) {
	h := Header3{Kind: 1}
	cases := map[string][]byte{
		"empty":        nil,
		"short":        make([]byte, Header3Len+1),
		"count zero":   append(h.Marshal(), 0, 0),
		"count 17":     append(h.Marshal(), 0, 17),
		"idx >= count": append(h.Marshal(), 3, 3),
	}
	for name, b := range cases {
		if _, err := DecodeFragment3(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func FuzzDecodeFragment3(f *testing.F) {
	h := Header3{Kind: 1, Sender: 2, Seq: 5}
	f.Add(Fragment3{Header: h, Idx: 0, Count: 1, Payload: []byte("x")}.Encode())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := DecodeFragment3(data)
		if err != nil {
			return
		}
		if out := fr.Encode(); !bytes.Equal(out, data) {
			t.Fatalf("round trip changed the fragment:\n in %x\nout %x", data, out)
		}
	})
}

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestSealOpenBindsHeader(t *testing.T) {
	h := Header3{Kind: Kind3Audio, Epoch: 1, Sender: 4, Stream: 0, Gen: 1, Seq: 9, TS: 160}
	ct, err := Seal3(h, testKey(1), []byte("voice"))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := Open3(h, testKey(1), ct); err != nil || string(pt) != "voice" {
		t.Fatalf("open: %q %v", pt, err)
	}
	if _, err := Open3(h, testKey(2), ct); !errors.Is(err, ErrOpen3) {
		t.Fatalf("wrong key: %v", err)
	}
	for name, mut := range map[string]func(*Header3){
		"sender": func(x *Header3) { x.Sender++ }, "seq": func(x *Header3) { x.Seq++ },
		"epoch": func(x *Header3) { x.Epoch++ }, "ts": func(x *Header3) { x.TS++ },
		"gen": func(x *Header3) { x.Gen++ }, "stream": func(x *Header3) { x.Stream++ },
		"kind": func(x *Header3) { x.Kind = Kind3Video },
	} {
		m := h
		mut(&m)
		if _, err := Open3(m, testKey(1), ct); !errors.Is(err, ErrOpen3) {
			t.Errorf("relabelled %s still opens: %v", name, err)
		}
	}
	if _, err := Seal3(h, make([]byte, 16), nil); !errors.Is(err, ErrBadKey3) {
		t.Fatalf("short key: %v", err)
	}
}

func TestRejoinUsesNewKey(t *testing.T) {
	epoch := testKey(7)
	var dev [32]byte
	dev[0] = 1
	a, _ := SenderKey3(epoch, dev, bytes.Repeat([]byte{1}, 16))
	b, _ := SenderKey3(epoch, dev, bytes.Repeat([]byte{2}, 16))
	if bytes.Equal(a, b) {
		t.Fatal("a rejoin with a new join_salt must derive a new key (seq restarts at 0)")
	}
	dev2 := dev
	dev2[0] = 2
	c, _ := SenderKey3(epoch, dev2, bytes.Repeat([]byte{1}, 16))
	if bytes.Equal(a, c) {
		t.Fatal("the key must depend on the sender device")
	}
	if _, err := SenderKey3(epoch, dev, make([]byte, 15)); !errors.Is(err, ErrBadKey3) {
		t.Fatalf("short salt: %v", err)
	}
}

func TestSplitAndReassemble(t *testing.T) {
	h := Header3{Kind: Kind3Video, Sender: 1, Seq: 3}
	for _, n := range []int{1, MaxFragmentPayload3, MaxFragmentPayload3 + 1, 5 * MaxFragmentPayload3, 16 * MaxFragmentPayload3} {
		ct := pattern(n, 1)
		frs, err := Split3(h, ct)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(frs) > MaxFragments3 {
			t.Fatalf("n=%d: %d fragments", n, len(frs))
		}
		clk := newFakeClock()
		r := NewReassembler3(clk.Now)
		var got []byte
		var done bool
		for i := len(frs) - 1; i >= 0; i-- { // out of order
			if len(frs[i].Encode()) > MaxDatagram3 {
				t.Fatalf("fragment over the datagram floor")
			}
			var gh Header3
			gh, got, done = r.Add(frs[i])
			if done && gh != h {
				t.Fatalf("header %+v", gh)
			}
		}
		if !done || !bytes.Equal(got, ct) {
			t.Fatalf("n=%d: reassembly done=%v", n, done)
		}
	}
	if _, err := Split3(h, make([]byte, 16*MaxFragmentPayload3+1)); !errors.Is(err, ErrUnitTooLarge3) {
		t.Fatalf("17 fragments: %v", err)
	}
}

func TestReassemblerBoundsAndExpiry(t *testing.T) {
	clk := newFakeClock()
	r := NewReassembler3(clk.Now)
	h := Header3{Kind: 1, Seq: 1}
	r.Add(Fragment3{Header: h, Idx: 0, Count: 2, Payload: []byte("a")})
	clk.Advance(2 * time.Second)
	// the half unit expired: its second half alone does not complete anything.
	if _, _, done := r.Add(Fragment3{Header: h, Idx: 1, Count: 2, Payload: []byte("b")}); done {
		t.Fatal("expired partial unit completed")
	}
	// a flood of incomplete units is bounded; a real unit still completes afterwards.
	for i := uint32(0); i < 10000; i++ {
		r.Add(Fragment3{Header: Header3{Kind: 1, Seq: 1000 + i}, Idx: 0, Count: 2, Payload: []byte("x")})
	}
	real := Header3{Kind: 1, Seq: 99999}
	r.Add(Fragment3{Header: real, Idx: 0, Count: 2, Payload: []byte("a")})
	if _, _, done := r.Add(Fragment3{Header: real, Idx: 1, Count: 2, Payload: []byte("b")}); !done {
		t.Fatal("a real unit does not complete after a flood of partial units")
	}
	// a fragment that disagrees on the count is dropped.
	h2 := Header3{Kind: 1, Seq: 5}
	r.Add(Fragment3{Header: h2, Idx: 0, Count: 2, Payload: []byte("a")})
	if _, _, done := r.Add(Fragment3{Header: h2, Idx: 1, Count: 3, Payload: []byte("b")}); done {
		t.Fatal("fragment with another count completed the unit")
	}
}

func fragsOf(h Header3, ct []byte, n int) []Fragment3 {
	size := (len(ct) + n - 1) / n
	out := make([]Fragment3, n)
	for i := range out {
		out[i] = Fragment3{Header: h, Idx: byte(i), Count: byte(n), Payload: ct[i*size : min((i+1)*size, len(ct))]}
	}
	return out
}

func TestForgedFragmentDoesNotShadowTheRealOne(t *testing.T) {
	clk := newFakeClock()
	r := NewReassembler3(clk.Now)
	h := Header3{Kind: Kind3Video, Sender: 2, Gen: 1, Seq: 7}
	real := pattern(200, 1)
	fake := pattern(200, 100)
	var got [][]byte
	collect := func(f Fragment3) {
		for _, a := range r.AddAll(f) {
			got = append(got, a.Ciphertext)
		}
	}
	for _, f := range fragsOf(h, fake, 2) { // the insider's fragments come first
		collect(f)
	}
	for _, f := range fragsOf(h, real, 2) {
		collect(f)
	}
	var sawReal bool
	for _, g := range got {
		if bytes.Equal(g, real) {
			sawReal = true
		}
	}
	if !sawReal {
		t.Fatalf("the real unit was never assembled (%d assemblies): a forged fragment shadowed it", len(got))
	}
}

func TestFragmentFloodOfOneSenderDoesNotEvictAnother(t *testing.T) {
	clk := newFakeClock()
	r := NewReassembler3(clk.Now)
	real := Header3{Kind: Kind3Video, Sender: 5, Gen: 1, Seq: 1}
	ct := pattern(100, 3)
	frs := fragsOf(real, ct, 2)
	r.AddAll(frs[0])
	for i := uint32(0); i < 300; i++ { // another member invents units for 300 ms
		r.AddAll(Fragment3{Header: Header3{Kind: Kind3Video, Sender: 6, Gen: 1, Seq: 1000 + i}, Idx: 0, Count: 2, Payload: []byte{1}})
		clk.Advance(time.Millisecond)
	}
	out := r.AddAll(frs[1])
	if len(out) != 1 || !bytes.Equal(out[0].Ciphertext, ct) {
		t.Fatalf("the real partial unit was evicted by another sender's flood: %d assemblies", len(out))
	}
}

func TestReassemblerDropsUnannouncedSenders(t *testing.T) {
	clk := newFakeClock()
	r := NewReassembler3(clk.Now)
	r.Known = func(sender, gen byte) bool { return sender == 1 }
	for i := uint32(0); i < 5000; i++ {
		r.AddAll(Fragment3{Header: Header3{Kind: Kind3Video, Sender: 9, Seq: i}, Idx: 0, Count: 2, Payload: []byte{1}})
	}
	if n := len(r.pending); n != 0 {
		t.Fatalf("%d pending units of an unannounced sender", n)
	}
	h := Header3{Kind: Kind3Video, Sender: 1, Seq: 1}
	frs := fragsOf(h, pattern(50, 1), 2)
	r.AddAll(frs[0])
	if out := r.AddAll(frs[1]); len(out) != 1 {
		t.Fatalf("announced sender: %d assemblies", len(out))
	}
}
