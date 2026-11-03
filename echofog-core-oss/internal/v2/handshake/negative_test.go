package handshake

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// seededRand is a deterministic io.Reader for the uniform-close tests.
type seededRand struct{ r *rand.ChaCha8 }

func newSeeded(seed byte) *seededRand {
	var s [32]byte
	s[0] = seed
	return &seededRand{rand.NewChaCha8(s)}
}

func (s *seededRand) Read(p []byte) (int, error) { return s.r.Read(p) }

// --- tampering at each step -------------------------------------------------------------------

func TestTamperedMessageEachStep(t *testing.T) {
	for _, xx := range []bool{false, true} {
		for step := 1; step <= 3; step++ {
			for _, where := range []string{"first", "middle", "last"} {
				w := newWorld(t)
				if xx {
					w.ic.Remote = nil
				}
				ini, _ := NewInitiator(w.ic, testCB)
				res := NewResponder(w.rc)
				flip := func(b []byte) []byte {
					b = append([]byte(nil), b...)
					switch where {
					case "first":
						b[0] ^= 0x01
					case "middle":
						b[len(b)/2] ^= 0x01
					default:
						b[len(b)-1] ^= 0x01
					}
					return b
				}
				name := map[bool]string{false: "XK", true: "XX"}[xx]
				hello, m1, _ := ini.Start()
				if step == 1 {
					m1 = flip(m1)
				}
				rep := res.HandleFirst(testCB, hello, m1, w.now)
				if step == 1 {
					if rep.Action == ActSend {
						// XX message 1 is not authenticated; the change must break message 2.
						if !xx {
							t.Fatalf("%s step 1 %s: accepted", name, where)
						}
						if _, _, err := ini.HandleMsg2(rep.Payload); err == nil {
							t.Fatalf("%s step 1 %s: not detected by message 2", name, where)
						}
					}
					continue
				}
				m2 := rep.Payload
				if step == 2 {
					m2 = flip(m2)
				}
				m3, _, err := ini.HandleMsg2(m2)
				if step == 2 {
					if err == nil {
						t.Fatalf("%s step 2 %s: accepted", name, where)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := res.HandleFinish(flip(m3)); err == nil {
					t.Fatalf("%s step 3 %s: accepted", name, where)
				}
			}
		}
	}
}

func TestTamperedHello(t *testing.T) {
	w := newWorld(t)
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	bad := append([]byte(nil), hello...)
	bad[len(bad)-1] ^= FlagResponderID // drop the responder-ID flag: rid becomes zero
	if rep := NewResponder(w.rc).HandleFirst(testCB, bad, m1, w.now); rep.Action != ActClose {
		t.Fatalf("hello flag change not detected: %v", rep.Action)
	}
	// A different channel binding (TLS terminated by a man in the middle).
	other := bytes.Repeat([]byte{0xcc}, 32)
	if rep := NewResponder(w.rc).HandleFirst(other, hello, m1, w.now); rep.Action != ActClose {
		t.Fatalf("channel binding mismatch not detected: %v", rep.Action)
	}
}

// --- downgrade -------------------------------------------------------------------------------

func TestDowngradeStrippedOffer(t *testing.T) {
	w := newWorld(t)
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	h, _ := ParseHello(hello)
	if len(h.Offered) != 2 || h.Chosen != SuiteXK {
		t.Fatalf("offered %v", h.Offered)
	}
	// 1. The attacker strips XX from the list; chosen stays XK: transcript mismatch.
	h1 := *h
	h1.Offered = []uint16{SuiteXK}
	b1, _ := h1.Marshal()
	if rep := NewResponder(w.rc).HandleFirst(testCB, b1, m1, w.now); rep.Action != ActClose {
		t.Fatalf("stripped offer accepted: %v", rep.Action)
	}
	// 2. The attacker pushes the choice down to XX.
	h2 := *h
	h2.Offered, h2.Chosen = []uint16{SuiteXX}, SuiteXX
	b2, _ := h2.Marshal()
	rep := NewResponder(w.rc).HandleFirst(testCB, b2, m1, w.now)
	if rep.Action == ActSend {
		if _, _, err := ini.HandleMsg2(rep.Payload); err == nil {
			t.Fatal("downgrade to XX completed")
		}
	}
	// 3. The responder enforces its own best suite: XX chosen while XK is offered is refused.
	w2 := newWorld(t)
	ini2, _ := NewInitiator(w2.ic, testCB)
	ini2.chosenOverride = SuiteXX
	hello3, m13, err := ini2.Start()
	if err != nil {
		t.Fatal(err)
	}
	rep = NewResponder(w2.rc).HandleFirst(testCB, hello3, m13, w2.now)
	if rep.Action != ActSendAndClose || rep.FrameType != FrameReject {
		t.Fatalf("non-best choice: %v", rep.Action)
	}
	r, err := ParseReject(rep.Payload)
	if err != nil || r.Reason != ReasonSuite || r.Detail != "0001,0002" {
		t.Fatalf("suite reject %+v", r)
	}
}

func TestForgedSuiteRejectCannotNarrow(t *testing.T) {
	w := newWorld(t)
	ini, _ := NewInitiator(w.ic, testCB)
	hello, _, _ := ini.Start()
	// A REJECT{suite} before Noise lists only XX; the retry must still offer both.
	forged := (&Reject{Reason: ReasonSuite, Detail: "0002"}).Marshal()
	hello2, m1, err := ini.HandleReject(forged)
	if err != nil {
		t.Fatal(err)
	}
	h1, _ := ParseHello(hello)
	h2, _ := ParseHello(hello2)
	if !equal16(h1.Offered, h2.Offered) || h2.Chosen != SuiteXX {
		t.Fatalf("retry offered %v chosen %d", h2.Offered, h2.Chosen)
	}
	// The real responder prefers XK, so the narrowed choice fails again ...
	rep := NewResponder(w.rc).HandleFirst(testCB, hello2, m1, w.now)
	if rep.FrameType != FrameReject {
		t.Fatalf("narrowed retry accepted: %v", rep.Action)
	}
	// ... and only one retry is allowed.
	if _, _, err := ini.HandleReject(rep.Payload); err == nil {
		t.Fatal("second retry allowed")
	}
}

func TestSuiteRejectRetryFindsCommonSuite(t *testing.T) {
	w := newWorld(t)
	w.rc.Suites = []uint16{SuiteXX} // a responder whose card is stale for the initiator
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	res := NewResponder(w.rc)
	rep := res.HandleFirst(testCB, hello, m1, w.now)
	if rep.FrameType != FrameReject || rep.Action != ActSendAndClose {
		t.Fatalf("want REJECT{suite}: %v", rep.Action)
	}
	hello2, m1b, err := ini.HandleReject(rep.Payload)
	if err != nil {
		t.Fatal(err)
	}
	rep = NewResponder(w.rc).HandleFirst(testCB, hello2, m1b, w.now)
	if rep.FrameType != FrameMsg {
		t.Fatalf("retry: %v %v", rep.Action, rep.Err)
	}
}

func equal16(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- wrong expected responder ID --------------------------------------------------------------

func TestWrongExpectedResponderIDXK(t *testing.T) {
	w := newWorld(t)
	// Unknown key-share: a card by another device that copies the responder's hs_key.
	copier := newTestIdentity(t, 5)
	fake := copier.card(t, CardOptions{HsKey: w.rID.hs.PublicKey().Bytes(), Scope: 1})
	info, err := (DefaultCardVerifier{}).VerifyCard(fake, w.now)
	if err != nil {
		t.Fatal(err)
	}
	w.ic.Remote = info
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	if rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now); rep.Action != ActClose {
		t.Fatalf("UKS: responder answered: %v", rep.Action)
	}
}

func TestWrongExpectedResponderIDXX(t *testing.T) {
	w := newWorld(t)
	wrong := newTestIdentity(t, 6).deviceID()
	w.ic.Remote, w.ic.ExpectedDeviceID = nil, &wrong
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
	if rep.Action != ActSend {
		t.Fatalf("responder: %v", rep.Action)
	}
	m3, _, err := ini.HandleMsg2(rep.Payload)
	if err == nil || m3 != nil {
		t.Fatal("wrong responder ID accepted; the initiator must not send message 3")
	}
}

func TestResponderCardMismatch(t *testing.T) {
	// XX: the card in P2 is valid but names another hs_key: the initiator aborts before message 3.
	w := newWorld(t)
	w.ic.Remote = nil
	w.rc.Card = newTestIdentity(t, 7).card(t, CardOptions{Scope: 1})
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
	if _, _, err := ini.HandleMsg2(rep.Payload); !errors.Is(err, ErrPeerCard) {
		t.Fatalf("card/hs mismatch: %v", err)
	}
}

func TestInitiatorCardMismatch(t *testing.T) {
	w := newWorld(t)
	w.ic.Card = newTestIdentity(t, 8).card(t, CardOptions{Scope: 1}) // hs_key is not the initiator's
	ini, _ := NewInitiator(w.ic, testCB)
	res := NewResponder(w.rc)
	hello, m1, _ := ini.Start()
	rep := res.HandleFirst(testCB, hello, m1, w.now)
	m3, _, err := ini.HandleMsg2(rep.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.HandleFinish(m3); !errors.Is(err, ErrPeerCard) {
		t.Fatalf("initiator card mismatch: %v", err)
	}
}

func TestHsKeyBoundToOtherDevice(t *testing.T) {
	w := newWorld(t)
	b := NewKeyBindings()
	w.rc.Bindings = b
	_, sr := w.handshake(t)
	if sr.Peer == nil {
		t.Fatal("no peer")
	}
	// Another device now presents a card with the same hs key.
	impostor := newTestIdentity(t, 4)
	w.ic.Card = impostor.card(t, CardOptions{HsKey: w.iID.hs.PublicKey().Bytes(), Scope: 1})
	ini, _ := NewInitiator(w.ic, testCB)
	res := NewResponder(w.rc)
	hello, m1, _ := ini.Start()
	rep := res.HandleFirst(testCB, hello, m1, w.now)
	m3, _, _ := ini.HandleMsg2(rep.Payload)
	if _, err := res.HandleFinish(m3); !errors.Is(err, ErrKeyBinding) {
		t.Fatalf("rebinding: %v", err)
	}
}

// --- hidden nodes ----------------------------------------------------------------------------

func hiddenWorld(t testing.TB) (*world, []byte) {
	w := newWorld(t)
	secret := bytes.Repeat([]byte{0x5e}, 16)
	w.rc.Hidden = true
	w.rc.Passes = passMap{string(PassID(secret)): secret}
	w.ic.AccessSecret = secret
	return w, secret
}

func TestHiddenNodeAcceptsValidProof(t *testing.T) {
	w, _ := hiddenWorld(t)
	si, sr := w.handshake(t)
	checkSessions(t, si, sr)
}

func TestHiddenNodeSilenceAndUniformClose(t *testing.T) {
	type tc struct {
		name string
		mod  func(w *world, hello, m1 []byte) ([]byte, []byte)
		pre  func(w *world)
	}
	cases := []tc{
		{"malformed hello", func(w *world, h, m []byte) ([]byte, []byte) { return h[:3], m }, nil},
		{"XX offered", nil, func(w *world) { w.ic.Remote, w.ic.AccessSecret = nil, nil }},
		{"tampered message 1", func(w *world, h, m []byte) ([]byte, []byte) {
			m = append([]byte(nil), m...)
			m[len(m)-1] ^= 1
			return h, m
		}, nil},
		{"no access proof", nil, func(w *world) { w.ic.AccessSecret = nil }},
		{"unknown pass", nil, func(w *world) { w.ic.AccessSecret = bytes.Repeat([]byte{0x11}, 16) }},
		{"wrong proof", nil, func(w *world) {
			// A secret with the right pass_id is impossible to forge; use a store mapping the
			// initiator's pass_id to another secret instead.
			w.rc.Passes = passMap{string(PassID(w.ic.AccessSecret)): bytes.Repeat([]byte{0x22}, 16)}
		}},
		{"truncated message 1", func(w *world, h, m []byte) ([]byte, []byte) { return h, m[:40] }, nil},
	}
	var delays []time.Duration
	for _, c := range cases {
		w, _ := hiddenWorld(t)
		if c.pre != nil {
			c.pre(w)
		}
		w.rc.Rand = newSeeded(42) // same randomness for every case
		ini, err := NewInitiator(w.ic, testCB)
		if err != nil {
			t.Fatal(err)
		}
		hello, m1, err := ini.Start()
		if err != nil {
			t.Fatal(err)
		}
		if c.mod != nil {
			hello, m1 = c.mod(w, hello, m1)
		}
		arrival := w.now
		rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, arrival)
		if rep.Action != ActSilentClose || rep.Payload != nil || rep.FrameType != 0 {
			t.Fatalf("%s: hidden node answered: %+v", c.name, rep)
		}
		t.Logf("%s: %v", c.name, rep.Err)
		d := rep.CloseAt.Sub(arrival)
		if d < 500*time.Millisecond || d > 3*time.Second {
			t.Fatalf("%s: close delay %v outside [0.5 s, 3 s]", c.name, d)
		}
		delays = append(delays, d)
	}
	// The delay does not depend on which check failed.
	for i := range delays {
		if delays[i] != delays[0] {
			t.Fatalf("close delay depends on the failure: %v", delays)
		}
	}
	// And it is spread over the window across connections.
	w, _ := hiddenWorld(t)
	w.ic.AccessSecret = nil
	w.rc.Rand = newSeeded(1)
	res := NewResponder(w.rc)
	min, max := 10*time.Second, time.Duration(0)
	for i := 0; i < 200; i++ {
		ini, _ := NewInitiator(w.ic, testCB)
		hello, m1, _ := ini.Start()
		d := res.HandleFirst(testCB, hello, m1, w.now).CloseAt.Sub(w.now)
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	if min > 800*time.Millisecond || max < 2700*time.Millisecond {
		t.Fatalf("delays not uniform over [0.5, 3] s: min %v max %v", min, max)
	}
}

func TestHiddenNodeNoPuzzleNoSuiteReject(t *testing.T) {
	w, _ := hiddenWorld(t)
	w.rc.Puzzle = &PuzzleConfig{Secret: bytes.Repeat([]byte{7}, 32), Difficulty: func() uint8 { return 16 }}
	w.rc.Suites = []uint16{SuiteXK}
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	if rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now); rep.FrameType != FrameMsg {
		t.Fatalf("hidden node demanded a puzzle: %v", rep.FrameType)
	}
	// A suite mismatch is a silent close, never REJECT{suite}.
	ini2, _ := NewInitiator(w.ic, testCB)
	ini2.chosenOverride = SuiteXX
	hello, m1, _ = ini2.Start()
	if rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now); rep.Action != ActSilentClose {
		t.Fatalf("hidden suite mismatch: %v", rep.Action)
	}
}

func TestHiddenNodeRejectsOnlyAfterProof(t *testing.T) {
	w, _ := hiddenWorld(t)
	w.now = fixedNow
	ini, _ := NewInitiator(w.ic, testCB)
	ini.timeOverride = fixedTime - 601
	hello, m1, _ := ini.Start()
	rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
	if rep.Action != ActSendAndClose || rep.FrameType != FrameMsg {
		t.Fatalf("after a valid proof the hidden node explains in P2: %v", rep.Action)
	}
	if _, _, err := ini.HandleMsg2(rep.Payload); !isReject(err, ReasonClockSkew) {
		t.Fatalf("want clock-skew reject: %v", err)
	}
}

// --- 0-RTT replay ----------------------------------------------------------------------------

func TestEarlyDataReplay(t *testing.T) {
	w := newWorld(t)
	calls := 0
	w.rc.Early = func(req []byte) ([]byte, bool) { calls++; return []byte("ok"), true }
	w.ic.Early = []byte("FIND_VALUE")
	w.ic.Rand = newSeeded(3)
	ini, _ := NewInitiator(w.ic, testCB)
	ini.timeOverride = fixedTime
	hello, m1, _ := ini.Start()
	res := NewResponder(w.rc)
	rep := res.HandleFirst(testCB, hello, m1, w.now)
	if _, si, err := ini.HandleMsg2(rep.Payload); err != nil || si.EarlyStatus != EarlyProcessed {
		t.Fatalf("first: %v", err)
	}
	// The same flight replayed within the window: the handshake may proceed, the early data not.
	w.now = w.now.Add(30 * time.Second)
	rep2 := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
	if rep2.Action != ActSend {
		t.Fatalf("replay: %v", rep2.Action)
	}
	p2 := decryptP2ForTest(t, w, hello, m1, rep2.Payload)
	if p2.EarlyStatus != EarlyIgnored || calls != 1 {
		t.Fatalf("replayed early data processed: status %d, calls %d", p2.EarlyStatus, calls)
	}
	// After 180 s the cache forgets the key, but the 60 s time window already refuses it.
	w.now = w.now.Add(200 * time.Second)
	rep3 := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
	if p := decryptP2ForTest(t, w, hello, m1, rep3.Payload); p.EarlyStatus != EarlyIgnored || calls != 1 {
		t.Fatalf("old early data processed: %d", p.EarlyStatus)
	}
}

func TestEarlyDataTimeWindow(t *testing.T) {
	w := newWorld(t)
	calls := 0
	w.rc.Early = func([]byte) ([]byte, bool) { calls++; return nil, true }
	w.ic.Early = []byte("q")
	ini, _ := NewInitiator(w.ic, testCB)
	ini.timeOverride = fixedTime - 61
	hello, m1, _ := ini.Start()
	rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
	_, si, err := ini.HandleMsg2(rep.Payload)
	if err != nil || si.EarlyStatus != EarlyIgnored || calls != 0 {
		t.Fatalf("early outside 60 s: %v %d", err, calls)
	}
}

func TestEarlyDataTooLarge(t *testing.T) {
	w := newWorld(t)
	w.ic.Early = make([]byte, MaxEarlyLen+1)
	if _, err := NewInitiator(w.ic, testCB); err == nil {
		t.Fatal("oversized early data accepted")
	}
}

// decryptP2ForTest re-runs the initiator side from the same ephemerals to read a P2 sent to a
// replayed flight. It uses the initiator's recorded noise state.
func decryptP2ForTest(t *testing.T, w *world, hello, m1, m2 []byte) *HsResp {
	t.Helper()
	p2, err := w.peekP2(hello, m1, m2)
	if err != nil {
		t.Fatal(err)
	}
	return p2
}

// --- clock skew and version ------------------------------------------------------------------

func TestClockSkewReject(t *testing.T) {
	for _, skew := range []int64{-601, 601} {
		w := newWorld(t)
		ini, _ := NewInitiator(w.ic, testCB)
		ini.timeOverride = uint64(fixedTime + skew)
		hello, m1, _ := ini.Start()
		rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
		if rep.Action != ActSendAndClose {
			t.Fatalf("skew %d: %v", skew, rep.Action)
		}
		_, _, err := ini.HandleMsg2(rep.Payload)
		var r *Reject
		if !errors.As(err, &r) || r.Reason != ReasonClockSkew || r.Min != uint32(fixedTime) {
			t.Fatalf("skew %d: %v", skew, err)
		}
	}
	// 600 s is still accepted.
	w := newWorld(t)
	ini, _ := NewInitiator(w.ic, testCB)
	ini.timeOverride = fixedTime - 600
	hello, m1, _ := ini.Start()
	rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
	if _, _, err := ini.HandleMsg2(rep.Payload); err != nil {
		t.Fatalf("600 s: %v", err)
	}
	// The learned offset is applied before the check.
	w = newWorld(t)
	w.rc.Offset = 700 * time.Second
	ini, _ = NewInitiator(w.ic, testCB)
	ini.timeOverride = fixedTime + 700
	hello, m1, _ = ini.Start()
	rep = NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
	if rep.Action != ActSend {
		t.Fatalf("offset not applied: %v", rep.Action)
	}
}

func TestInitiatorChecksResponderClock(t *testing.T) {
	w := newWorld(t)
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	w.rc.Now = func() time.Time { return fixedNow.Add(-700 * time.Second) }
	w.rc.Offset = 700 * time.Second // the responder's corrected clock still matches P1.time
	rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, fixedNow)
	if rep.Action != ActSend {
		t.Fatalf("responder: %v", rep.Action)
	}
	w.rc.Offset = 0
	w.rc.Now = func() time.Time { return fixedNow }
	// Re-run with a responder whose P2.time is off by 700 s.
	ini, _ = NewInitiator(w.ic, testCB)
	hello, m1, _ = ini.Start()
	res := NewResponder(w.rc)
	res.timeOverride = fixedTime + 700
	rep = res.HandleFirst(testCB, hello, m1, fixedNow)
	if _, _, err := ini.HandleMsg2(rep.Payload); !errors.Is(err, ErrClockSkew) {
		t.Fatalf("initiator accepted a skewed responder: %v", err)
	}
}

func TestVersionReject(t *testing.T) {
	w := newWorld(t)
	ini, _ := NewInitiator(w.ic, testCB)
	ini.protoOverride = 199
	hello, m1, _ := ini.Start()
	rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
	_, _, err := ini.HandleMsg2(rep.Payload)
	var r *Reject
	if !errors.As(err, &r) || r.Reason != ReasonVersion || r.Min != 200 || r.Max != 200 {
		t.Fatalf("version: %v", err)
	}
	// A higher minor or major proto_ver is accepted.
	ini, _ = NewInitiator(w.ic, testCB)
	ini.protoOverride = 207
	hello, m1, _ = ini.Start()
	rep = NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now)
	if _, _, err := ini.HandleMsg2(rep.Payload); err != nil {
		t.Fatalf("207: %v", err)
	}
}

func TestInitiatorRejectsUnofferedSelection(t *testing.T) {
	w := newWorld(t)
	w.ic.Features = []uint32{1}
	w.ic.Services = []ServiceOffer{{ID: 1, Versions: []uint32{2}}}
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	res := NewResponder(w.rc)
	res.selectOverride = func(p *HsResp) { p.Features = append(p.Features, 5) }
	rep := res.HandleFirst(testCB, hello, m1, w.now)
	if _, _, err := ini.HandleMsg2(rep.Payload); err == nil || !strings.Contains(err.Error(), "feature") {
		t.Fatalf("unoffered feature: %v", err)
	}
	ini, _ = NewInitiator(w.ic, testCB)
	hello, m1, _ = ini.Start()
	res = NewResponder(w.rc)
	res.selectOverride = func(p *HsResp) { p.Services = []ServiceChoice{{1, 3}} }
	rep = res.HandleFirst(testCB, hello, m1, w.now)
	if _, _, err := ini.HandleMsg2(rep.Payload); err == nil || !strings.Contains(err.Error(), "service") {
		t.Fatalf("unoffered service version: %v", err)
	}
}

// --- puzzle ----------------------------------------------------------------------------------

func TestPuzzleNegative(t *testing.T) {
	w := newWorld(t)
	w.rc.Puzzle = &PuzzleConfig{Secret: bytes.Repeat([]byte{7}, 32), Difficulty: func() uint8 { return 10 }}
	res := NewResponder(w.rc)
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	rep := res.HandleFirst(testCB, hello, m1, w.now)
	retry, _ := ParseRetry(rep.Payload)
	// Expired cookie (two minutes later).
	hello2, m1b, err := ini.HandleRetry(rep.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.HandleFirst(testCB, hello2, m1b, w.now.Add(2*time.Minute)); r.FrameType != FrameRetry {
		t.Fatalf("expired cookie: %v", r.FrameType)
	}
	// A wrong nonce.
	h, _ := ParseHello(hello2)
	for PuzzleOK(h.Cookie, testCB, h.Nonce, retry.Difficulty) {
		h.Nonce++
	}
	bad, _ := h.Marshal()
	if r := res.HandleFirst(testCB, bad, m1b, w.now); r.FrameType != FrameRetry {
		t.Fatalf("bad nonce: %v", r.FrameType)
	}
	// The cookie is bound to cb.
	if r := res.HandleFirst(bytes.Repeat([]byte{1}, 32), hello2, m1b, w.now); r.FrameType != FrameRetry {
		t.Fatalf("cookie from another connection: %v", r.FrameType)
	}
	// Difficulty above 24 is refused by the initiator.
	var r Retry
	r.Difficulty = 25
	if _, _, err := ini.HandleRetry(append(r.Cookie[:], 25)); err == nil {
		t.Fatal("difficulty 25 accepted")
	}
}

// --- payload rules ---------------------------------------------------------------------------

func TestXXInitiatorPayloadRules(t *testing.T) {
	// A P1 with access in XX is refused by the responder (it travels in clear).
	w := newWorld(t)
	w.ic.Remote = nil
	ini, _ := NewInitiator(w.ic, testCB)
	ini.forceAccess = &Access{PassID: make([]byte, 8), Proof: make([]byte, 32)}
	hello, m1, _ := ini.Start()
	if rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now); rep.Action != ActClose {
		t.Fatalf("XX with access: %v", rep.Action)
	}
}

func TestHandleFinishRequiresCard(t *testing.T) {
	w := newWorld(t)
	w.ic.Card = nil
	if _, err := NewInitiator(w.ic, testCB); err == nil {
		t.Fatal("initiator without card")
	}
}

func TestOutOfOrderCalls(t *testing.T) {
	w := newWorld(t)
	ini, _ := NewInitiator(w.ic, testCB)
	if _, _, err := ini.HandleMsg2(make([]byte, 100)); err == nil {
		t.Fatal("message 2 before start")
	}
	res := NewResponder(w.rc)
	if _, err := res.HandleFinish(make([]byte, 64)); err == nil {
		t.Fatal("message 3 before message 1")
	}
}
