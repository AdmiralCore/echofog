package media

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

type constReader byte

func (c constReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c)
	}
	return len(p), nil
}

func TestSenderRefusesMediaWithoutEpochSecret(t *testing.T) {
	clk := newFakeClock()
	ek := NewEpochKeys(clk.Now)
	priv, _ := testMediaKey(1)
	s, err := NewSender3(Sender3Config{Index: 3, Device: [32]byte{1}, JoinSalt: bytes.Repeat([]byte{1}, 16),
		Gen: 1, MediaPriv: priv, Epochs: ek, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(Kind3Audio, 0, 0, []byte("hi")); !errors.Is(err, ErrNoEpoch) {
		t.Fatalf("send before the epoch secret: %v, want ErrNoEpoch", err)
	}
	ek.Install(0, testKey(9), time.Time{})
	if _, err := s.Send(Kind3Audio, 0, 0, []byte("hi")); err != nil {
		t.Fatalf("send after the epoch secret: %v", err)
	}
}

func TestNewSender3ValidatesInput(t *testing.T) {
	clk := newFakeClock()
	priv, _ := testMediaKey(1)
	ok := Sender3Config{JoinSalt: make([]byte, 16), MediaPriv: priv, Epochs: NewEpochKeys(clk.Now), Now: clk.Now}
	for name, mut := range map[string]func(*Sender3Config){
		"short salt": func(c *Sender3Config) { c.JoinSalt = make([]byte, 8) },
		"no key":     func(c *Sender3Config) { c.MediaPriv = nil },
		"no epochs":  func(c *Sender3Config) { c.Epochs = nil },
	} {
		c := ok
		mut(&c)
		if _, err := NewSender3(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// endToEnd wires one sender to one receiver through the full v3 path.
func TestEndToEndSendReceive(t *testing.T) {
	clk := newFakeClock()
	room := testKey(4)
	ek := NewEpochKeys(clk.Now)
	ek.Install(0, Epoch0Secret(room), time.Time{})
	priv, pub := testMediaKey(1)
	dev := [32]byte{7}
	salt := bytes.Repeat([]byte{5}, 16)
	s, err := NewSender3(Sender3Config{Index: 2, Device: dev, JoinSalt: salt, Gen: 4, MediaPriv: priv, Epochs: ek, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	rx := NewReceiver(ReceiverConfig{Now: clk.Now,
		MediaKey: func(sender, gen byte) (ed25519.PublicKey, bool) {
			return pub, sender == 2 && gen == 4
		}})
	re := NewReassembler3(clk.Now)
	key, _ := SenderKey3(Epoch0Secret(room), dev, salt)
	var verified int
	for i := 0; i < 10; i++ {
		plain := pattern(3000, byte(i)) // 3 fragments
		sent, err := s.Send(Kind3Video, 1, uint32(i), plain)
		if err != nil {
			t.Fatal(err)
		}
		var hd Header3
		var ct []byte
		var done bool
		for _, fr := range sent.Fragments {
			f, err := DecodeFragment3(fr)
			if err != nil {
				t.Fatal(err)
			}
			hd, ct, done = re.Add(f)
		}
		if !done {
			t.Fatal("unit did not reassemble")
		}
		if pt, err := Open3(hd, key, ct); err != nil || !bytes.Equal(pt, plain) {
			t.Fatalf("unit %d does not open: %v", i, err)
		}
		rx.OnUnit(hd, ct)
		for _, a := range sent.Auths {
			res, err := rx.OnAuth(a)
			if err != nil {
				t.Fatal(err)
			}
			verified += len(res.Verified)
		}
	}
	if verified != 8 { // one batch of 8 video units
		t.Fatalf("verified %d, want 8", verified)
	}
}

func TestRejoinNeverReusesNonceUnderOneKey(t *testing.T) {
	clk := newFakeClock()
	ek := NewEpochKeys(clk.Now)
	ek.Install(0, testKey(1), time.Time{})
	priv, _ := testMediaKey(1)
	mk := func(salt byte) *Sender3 {
		s, err := NewSender3(Sender3Config{Index: 1, Device: [32]byte{1}, JoinSalt: bytes.Repeat([]byte{salt}, 16),
			Gen: 1, MediaPriv: priv, Epochs: ek, Now: clk.Now})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := mk(1), mk(2)
	sa, _ := a.Send(Kind3Audio, 0, 0, []byte("same plaintext"))
	sb, _ := b.Send(Kind3Audio, 0, 0, []byte("same plaintext"))
	// both start at seq 0 in the same epoch with the same plaintext: ciphertexts must differ.
	if bytes.Equal(sa.Fragments[0][Header3Len+2:], sb.Fragments[0][Header3Len+2:]) {
		t.Fatal("a rejoin in the same epoch reused the key stream")
	}
}

func TestEpochKeysSwitchAndGrace(t *testing.T) {
	clk := newFakeClock()
	ek := NewEpochKeys(clk.Now)
	ek.Install(0, testKey(1), time.Time{})
	ek.Install(1, testKey(2), clk.Now().Add(2*time.Second))
	if e, s, ok := ek.Current(); !ok || e != 0 || !bytes.Equal(s, testKey(1)) {
		t.Fatalf("before the switch: %d %v", e, ok)
	}
	if s, ok := ek.Secret(1); !ok || !bytes.Equal(s, testKey(2)) {
		t.Fatal("the next epoch secret is known before the switch (receivers must open early senders)")
	}
	clk.Advance(2 * time.Second)
	if e, _, _ := ek.Current(); e != 1 {
		t.Fatalf("after the switch epoch %d", e)
	}
	if _, ok := ek.Secret(0); !ok {
		t.Fatal("the previous epoch must stay valid for in-flight media")
	}
	clk.Advance(EpochGrace3 + time.Millisecond)
	if _, ok := ek.Secret(0); ok {
		t.Fatal("the previous epoch secret is kept past the grace period")
	}
	if _, ok := ek.Secret(5); ok {
		t.Fatal("unknown epoch")
	}
}

func TestCoordinatorRotation(t *testing.T) {
	members := [][32]byte{{1}, {2}, {3}}
	lo, err := NewEpochCoordinator(constReader(0)).Rotate(members)
	if err != nil {
		t.Fatal(err)
	}
	hi, err := NewEpochCoordinator(constReader(0xff)).Rotate(members)
	if err != nil {
		t.Fatal(err)
	}
	if lo.Delay != 1500*time.Millisecond {
		t.Fatalf("lowest delay %v, want 1.5 s", lo.Delay)
	}
	if hi.Delay < 2490*time.Millisecond || hi.Delay > 2500*time.Millisecond {
		t.Fatalf("highest delay %v, want just under 2.5 s", hi.Delay)
	}
	if lo.Epoch != 1 || len(lo.Recipients) != 3 {
		t.Fatalf("rotation %+v", lo)
	}

	// kick: the next rotation is drawn for the remaining members only and has a new secret.
	c := NewEpochCoordinator(rngCounter())
	r1, _ := c.Rotate(members)
	r2, _ := c.Rotate(members[:2])
	if r2.Epoch != r1.Epoch+1 || r1.Secret == r2.Secret {
		t.Fatalf("epochs/secrets not fresh: %+v %+v", r1, r2)
	}
	for _, rcpt := range r2.Recipients {
		if rcpt == members[2] {
			t.Fatal("a kicked member receives the new epoch secret")
		}
	}
	// recipients are a copy: changing them does not touch the caller's slice.
	r2.Recipients[0] = [32]byte{9}
	if members[0] == ([32]byte{9}) {
		t.Fatal("recipients alias the argument")
	}
}

func TestEpochSecretNotDerivableFromRoomAfterRotation(t *testing.T) {
	room := testKey(3)
	c := NewEpochCoordinator(rngCounter())
	r, _ := c.Rotate([][32]byte{{1}})
	if bytes.Equal(r.Secret[:], Epoch0Secret(room)) {
		t.Fatal("rotated secret equals epoch 0")
	}
}

type counterReader struct{ n byte }

func rngCounter() *counterReader { return &counterReader{} }

func (c *counterReader) Read(p []byte) (int, error) {
	for i := range p {
		c.n = c.n*31 + 7
		p[i] = c.n
	}
	return len(p), nil
}

func TestSenderStopsBeforeSeqWraps(t *testing.T) {
	clk := newFakeClock()
	ek := NewEpochKeys(clk.Now)
	ek.Install(0, testKey(1), time.Time{})
	priv, _ := testMediaKey(1)
	s, err := NewSender3(Sender3Config{JoinSalt: make([]byte, 16), MediaPriv: priv, Epochs: ek, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	s.seq[streamKind{0, Kind3Audio}] = ^uint32(0)
	// a wrapped seq would reuse a nonce under the same key: the sender refuses and a rejoin
	// (new join_salt, new key) is needed.
	if _, err := s.Send(Kind3Audio, 0, 0, []byte("x")); !errors.Is(err, ErrSeqExhausted) {
		t.Fatalf("send at seq 2^32-1: %v, want ErrSeqExhausted", err)
	}
}

func TestSenderKeyFollowsReinstalledEpochNumber(t *testing.T) {
	// epoch is one byte and wraps after 256 rotations: the same number may carry another secret.
	clk := newFakeClock()
	ek := NewEpochKeys(clk.Now)
	priv, _ := testMediaKey(1)
	dev := [32]byte{4}
	salt := bytes.Repeat([]byte{6}, 16)
	s, _ := NewSender3(Sender3Config{Device: dev, JoinSalt: salt, MediaPriv: priv, Epochs: ek, Now: clk.Now})
	ek.Install(7, testKey(1), time.Time{})
	s.Send(Kind3Audio, 0, 0, []byte("a"))
	clk.Advance(time.Minute)
	ek.Install(7, testKey(2), time.Time{})
	sent, err := s.Send(Kind3Audio, 0, 0, []byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := DecodeFragment3(sent.Fragments[0])
	want, _ := SenderKey3(testKey(2), dev, salt)
	if _, err := Open3(f.Header, want, f.Payload); err != nil {
		t.Fatalf("unit sealed under a stale key: %v", err)
	}
}
