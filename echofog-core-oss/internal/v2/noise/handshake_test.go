package noise

import (
	"bytes"
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/mlkem/mlkemtest"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
)

func genKey(t testing.TB) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func x25519Key(t testing.TB, sk []byte) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(sk)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type pair struct {
	i, r *HandshakeState
}

func newPair(t testing.TB, p *Pattern, prologue []byte) pair {
	t.Helper()
	is, rs := genKey(t), genKey(t)
	ic := Config{Pattern: p, Initiator: true, Prologue: prologue, Static: is}
	rc := Config{Pattern: p, Initiator: false, Prologue: prologue, Static: rs}
	if p.PreResponderStatic {
		ic.RemoteStatic = rs.PublicKey().Bytes()
	}
	i, err := NewHandshake(ic)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewHandshake(rc)
	if err != nil {
		t.Fatal(err)
	}
	return pair{i, r}
}

// run executes the whole pattern with the given payloads and returns the message bytes.
func run(t testing.TB, pp pair, payloads [3][]byte) [3][]byte {
	t.Helper()
	var msgs [3][]byte
	order := []struct{ w, r *HandshakeState }{{pp.i, pp.r}, {pp.r, pp.i}, {pp.i, pp.r}}
	for n, o := range order {
		m, err := o.w.WriteMessage(nil, payloads[n])
		if err != nil {
			t.Fatalf("write %d: %v", n+1, err)
		}
		got, err := o.r.ReadMessage(m)
		if err != nil {
			t.Fatalf("read %d: %v", n+1, err)
		}
		if !bytes.Equal(got, payloads[n]) {
			t.Fatalf("payload %d mismatch", n+1)
		}
		msgs[n] = m
	}
	return msgs
}

func TestRoundTripBothPatterns(t *testing.T) {
	for _, p := range []*Pattern{XKhfs, XXhfs} {
		t.Run(p.Name, func(t *testing.T) {
			pp := newPair(t, p, []byte("prologue"))
			run(t, pp, [3][]byte{[]byte("p1"), []byte("p2"), []byte("p3")})
			if !pp.i.Complete() || !pp.r.Complete() {
				t.Fatal("not complete")
			}
			if !bytes.Equal(pp.i.H(), pp.r.H()) || !bytes.Equal(pp.i.CK(), pp.r.CK()) {
				t.Fatal("final h/ck differ")
			}
			i1, i2, err := pp.i.Split()
			if err != nil {
				t.Fatal(err)
			}
			r1, r2, err := pp.r.Split()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(i1.Key(), r1.Key()) || !bytes.Equal(i2.Key(), r2.Key()) || bytes.Equal(i1.Key(), i2.Key()) {
				t.Fatal("transport keys")
			}
			if !bytes.Equal(pp.r.RemoteStatic(), pp.i.LocalStatic()) || !bytes.Equal(pp.i.RemoteStatic(), pp.r.LocalStatic()) {
				t.Fatal("remote statics")
			}
		})
	}
}

func TestMessageSizesWithoutPayload(t *testing.T) {
	// handshake.md §4.1: 1232, 1152, 64 bytes for XK; XX: 1216, 1200, 64.
	want := map[string][3]int{XKhfs.Name: {1232, 1152, 64}, XXhfs.Name: {1216, 1200, 64}}
	for _, p := range []*Pattern{XKhfs, XXhfs} {
		pp := newPair(t, p, nil)
		msgs := run(t, pp, [3][]byte{})
		for n := range msgs {
			if len(msgs[n]) != want[p.Name][n] {
				t.Errorf("%s message %d: %d bytes, want %d", p.Name, n+1, len(msgs[n]), want[p.Name][n])
			}
		}
	}
}

func TestPrologueMismatchFails(t *testing.T) {
	for _, p := range []*Pattern{XKhfs, XXhfs} {
		is, rs := genKey(t), genKey(t)
		ic := Config{Pattern: p, Initiator: true, Prologue: []byte("A"), Static: is}
		if p.PreResponderStatic {
			ic.RemoteStatic = rs.PublicKey().Bytes()
		}
		i, _ := NewHandshake(ic)
		r, _ := NewHandshake(Config{Pattern: p, Prologue: []byte("B"), Static: rs})
		m1, _ := i.WriteMessage(nil, []byte("x"))
		_, err1 := r.ReadMessage(m1)
		if p == XKhfs {
			if err1 == nil {
				t.Fatal("XK: prologue mismatch not detected in message 1")
			}
			continue
		}
		// XX message 1 has no key: the mismatch shows in message 2.
		if err1 != nil {
			t.Fatal(err1)
		}
		m2, _ := r.WriteMessage(nil, nil)
		if _, err := i.ReadMessage(m2); err == nil {
			t.Fatal("XX: prologue mismatch not detected in message 2")
		}
	}
}

func TestWrongResponderStaticXK(t *testing.T) {
	is, rs, other := genKey(t), genKey(t), genKey(t)
	i, _ := NewHandshake(Config{Pattern: XKhfs, Initiator: true, Static: is, RemoteStatic: other.PublicKey().Bytes()})
	r, _ := NewHandshake(Config{Pattern: XKhfs, Static: rs})
	m1, _ := i.WriteMessage(nil, nil)
	if _, err := r.ReadMessage(m1); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("want decrypt error, got %v", err)
	}
}

func TestTamperEveryRegion(t *testing.T) {
	for _, p := range []*Pattern{XKhfs, XXhfs} {
		for step := 0; step < 3; step++ {
			// Positions: first byte, a byte inside the KEM part, the last byte.
			for _, pos := range []int{0, 100, -1} {
				pp := newPair(t, p, []byte("pro"))
				payloads := [3][]byte{[]byte("one"), []byte("two"), []byte("three")}
				order := []struct{ w, r *HandshakeState }{{pp.i, pp.r}, {pp.r, pp.i}, {pp.i, pp.r}}
				failed := false
				for n := 0; n < 3 && !failed; n++ {
					m, err := order[n].w.WriteMessage(nil, payloads[n])
					if err != nil {
						t.Fatal(err)
					}
					if n == step {
						j := pos
						if j < 0 || j >= len(m) {
							j = len(m) - 1
						}
						m[j] ^= 0x40
					}
					_, err = order[n].r.ReadMessage(m)
					if n == step {
						if err != nil {
							failed = true
							break
						}
						// XX message 1 is unauthenticated: the change must surface in message 2.
						if !(p == XXhfs && step == 0) {
							t.Fatalf("%s step %d pos %d: tampering not detected", p.Name, step+1, pos)
						}
						continue
					}
					if err != nil {
						if p == XXhfs && step == 0 && n == 1 {
							failed = true
							break
						}
						t.Fatalf("%s step %d: unexpected error at message %d: %v", p.Name, step+1, n+1, err)
					}
				}
				if !failed {
					t.Fatalf("%s step %d pos %d: tampering never detected", p.Name, step+1, pos)
				}
			}
		}
	}
}

func TestTruncatedAndOversized(t *testing.T) {
	pp := newPair(t, XKhfs, nil)
	m1, _ := pp.i.WriteMessage(nil, nil)
	if _, err := pp.r.ReadMessage(m1[:100]); err == nil {
		t.Fatal("truncated message 1 accepted")
	}
	pp = newPair(t, XKhfs, nil)
	if _, err := pp.i.WriteMessage(nil, make([]byte, MaxMessageLen)); err == nil {
		t.Fatal("oversized message written")
	}
	pp = newPair(t, XKhfs, nil)
	if _, err := pp.r.ReadMessage(make([]byte, MaxMessageLen+1)); err == nil {
		t.Fatal("oversized message read")
	}
}

func TestOutOfTurn(t *testing.T) {
	pp := newPair(t, XKhfs, nil)
	if _, err := pp.r.WriteMessage(nil, nil); !errors.Is(err, ErrState) {
		t.Fatalf("responder wrote first: %v", err)
	}
	if _, err := pp.i.ReadMessage(make([]byte, 64)); !errors.Is(err, ErrState) {
		t.Fatalf("initiator read first: %v", err)
	}
	pp = newPair(t, XKhfs, nil)
	run(t, pp, [3][]byte{})
	if _, err := pp.i.WriteMessage(nil, nil); !errors.Is(err, ErrState) {
		t.Fatalf("write after completion: %v", err)
	}
}

func TestLowOrderEphemeralRejected(t *testing.T) {
	pp := newPair(t, XKhfs, nil)
	m1, _ := pp.i.WriteMessage(nil, nil)
	for i := 0; i < 32; i++ {
		m1[i] = 0 // u = 0: X25519 output all zero
	}
	if _, err := pp.r.ReadMessage(m1); err == nil {
		t.Fatal("all-zero DH accepted")
	}
}

func TestInvalidKEMKeyRejected(t *testing.T) {
	// XX message 1: e(32) ‖ e1(1184) in clear. Coefficients ≥ q fail the FIPS 203 check.
	pp := newPair(t, XXhfs, nil)
	m1, _ := pp.i.WriteMessage(nil, nil)
	for i := 32; i < 32+12; i++ {
		m1[i] = 0xff
	}
	if _, err := pp.r.ReadMessage(m1); !errors.Is(err, ErrBadKEMKey) {
		t.Fatalf("invalid ML-KEM key: %v", err)
	}
}

func TestDeterministicInputsMatchVectors(t *testing.T) {
	v := loadVector(t, "handshake.json")
	in := obj(t, v, "deferred_transcripts", "inputs_xk")
	ie := x25519Key(t, hexField(t, in, "initiator_e_sk"))
	if !bytes.Equal(ie.PublicKey().Bytes(), hexField(t, in, "initiator_e_pub")) {
		t.Fatal("initiator e pub")
	}
	seed := hexField(t, in, "initiator_e1_seed_dz")
	dk, err := mlkem.NewDecapsulationKey768(seed)
	if err != nil {
		t.Fatal(err)
	}
	ekHash := sha256.Sum256(dk.EncapsulationKey().Bytes())
	if !bytes.Equal(ekHash[:], hexField(t, in, "initiator_e1_ek_sha256")) {
		t.Fatal("ek hash")
	}
	m := hexField(t, in, "encaps_randomness_m")
	var gotCT, gotSS []byte
	encap := func(ek *mlkem.EncapsulationKey768) ([]byte, []byte, error) {
		ss, ct, err := mlkemtest.Encapsulate768(ek, m)
		gotCT, gotSS = ct, append([]byte(nil), ss...) // the handshake wipes ss
		return ss, ct, err
	}
	card := loadVector(t, "node-card.json")
	rs := x25519Key(t, hexField(t, card, "hs_sk"))
	i, err := NewHandshake(Config{Pattern: XKhfs, Initiator: true, Static: x25519Key(t, hexField(t, in, "initiator_s_sk")),
		RemoteStatic: rs.PublicKey().Bytes(), Ephemeral: ie, KEMSeed: seed})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewHandshake(Config{Pattern: XKhfs, Static: rs, Ephemeral: x25519Key(t, hexField(t, in, "responder_e_sk")), KEMEncapsulate: encap})
	if err != nil {
		t.Fatal(err)
	}
	run(t, pair{i, r}, [3][]byte{})
	ctHash := sha256.Sum256(gotCT)
	if !bytes.Equal(ctHash[:], hexField(t, in, "ekem1_ct_sha256")) {
		t.Fatal("ct hash")
	}
	if !bytes.Equal(gotSS, hexField(t, in, "ekem1_ss")) {
		t.Fatal("ss")
	}
}

func TestDeterministicRunsAreIdentical(t *testing.T) {
	mk := func() [3][]byte {
		is, rs, ie, re := x25519Key(t, bytes.Repeat([]byte{1}, 32)), x25519Key(t, bytes.Repeat([]byte{2}, 32)),
			x25519Key(t, bytes.Repeat([]byte{3}, 32)), x25519Key(t, bytes.Repeat([]byte{4}, 32))
		m := bytes.Repeat([]byte{5}, 32)
		enc := func(ek *mlkem.EncapsulationKey768) ([]byte, []byte, error) { return mlkemtest.Encapsulate768(ek, m) }
		i, _ := NewHandshake(Config{Pattern: XXhfs, Initiator: true, Static: is, Ephemeral: ie, KEMSeed: bytes.Repeat([]byte{6}, 64)})
		r, _ := NewHandshake(Config{Pattern: XXhfs, Static: rs, Ephemeral: re, KEMEncapsulate: enc})
		return run(t, pair{i, r}, [3][]byte{[]byte("a"), []byte("b"), []byte("c")})
	}
	a, b := mk(), mk()
	for n := range a {
		if !bytes.Equal(a[n], b[n]) {
			t.Fatalf("message %d differs between runs", n+1)
		}
	}
}

func TestHashBeforePayload(t *testing.T) {
	pp := newPair(t, XKhfs, nil)
	var hw []byte
	m1, err := pp.i.WriteMessageFunc(nil, func(h []byte) ([]byte, error) {
		hw = append([]byte(nil), h...)
		return []byte("proof-bearing payload"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pp.r.ReadMessage(m1); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hw, pp.r.HashBeforePayload()) || !bytes.Equal(hw, pp.i.HashBeforePayload()) {
		t.Fatal("h before payload differs")
	}
}

func TestTraceCoversEveryToken(t *testing.T) {
	var tokens []string
	is, rs := genKey(t), genKey(t)
	tr := func(e TraceEvent) { tokens = append(tokens, e.Token) }
	i, _ := NewHandshake(Config{Pattern: XKhfs, Initiator: true, Static: is, RemoteStatic: rs.PublicKey().Bytes(), Trace: tr})
	r, _ := NewHandshake(Config{Pattern: XKhfs, Static: rs})
	run(t, pair{i, r}, [3][]byte{})
	want := []string{"init", "prologue", "pre:s", "e", "e1", "es", "payload", "e", "ee", "ekem1", "payload", "s", "se", "payload"}
	if len(tokens) != len(want) {
		t.Fatalf("trace %v, want %v", tokens, want)
	}
	for n := range want {
		if tokens[n] != want[n] {
			t.Fatalf("trace %v, want %v", tokens, want)
		}
	}
}
