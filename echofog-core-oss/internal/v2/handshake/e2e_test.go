package handshake

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

var testCB = bytes.Repeat([]byte{0xcb}, 32)

type passMap map[string][]byte

func (p passMap) LookupPass(id []byte, _ time.Time) ([]byte, bool) {
	s, ok := p[string(id)]
	return s, ok
}

type world struct {
	rID, iID *testIdentity
	rCard    []byte
	rInfo    *CardInfo
	iCard    []byte
	now      time.Time
	rc       ResponderConfig
	ic       InitiatorConfig
}

func allServices() []ServiceOffer {
	var s []ServiceOffer
	for id := uint32(1); id <= 8; id++ {
		s = append(s, ServiceOffer{ID: id, Versions: []uint32{2}})
	}
	return s
}

func newWorld(t testing.TB) *world {
	t.Helper()
	w := &world{rID: newTestIdentity(t, 1), iID: newTestIdentity(t, 2), now: fixedNow}
	w.rCard = w.rID.card(t, CardOptions{Scope: 1})
	var err error
	if w.rInfo, err = (DefaultCardVerifier{}).VerifyCard(w.rCard, w.now); err != nil {
		t.Fatal(err)
	}
	w.iCard = w.iID.card(t, CardOptions{Scope: 1})
	clock := func() time.Time { return w.now }
	w.rc = ResponderConfig{
		Static:   w.rID.hs,
		DeviceID: w.rID.deviceID(),
		Card:     w.rCard,
		CardSeq:  fixedTime,
		Features: []uint32{1, 3, 4, 5, 6},
		Services: []ServiceOffer{{ID: 1, Versions: []uint32{2}}, {ID: 2, Versions: []uint32{2}}, {ID: 3, Versions: []uint32{2}}},
		Now:      clock,
		Replay:   NewReplayCache(),
		Observed: []byte{4, 192, 0, 2, 33, 0xc8, 0x22},
	}
	w.ic = InitiatorConfig{
		Static:   w.iID.hs,
		Card:     w.iCard,
		Remote:   w.rInfo,
		Features: []uint32{1, 3, 4, 5},
		Services: allServices(),
		Now:      clock,
	}
	return w
}

// handshake runs a full handshake between fresh instances and returns both sessions.
func (w *world) handshake(t testing.TB) (*Session, *Session) {
	t.Helper()
	ini, err := NewInitiator(w.ic, testCB)
	if err != nil {
		t.Fatal(err)
	}
	res := NewResponder(w.rc)
	hello, m1, err := ini.Start()
	if err != nil {
		t.Fatal(err)
	}
	rep := res.HandleFirst(testCB, hello, m1, w.now)
	if rep.Action != ActSend || rep.FrameType != FrameMsg {
		t.Fatalf("responder reply %+v (%v)", rep.Action, rep.Err)
	}
	m3, si, err := ini.HandleMsg2(rep.Payload)
	if err != nil {
		t.Fatal(err)
	}
	sr, err := res.HandleFinish(m3)
	if err != nil {
		t.Fatal(err)
	}
	return si, sr
}

func checkSessions(t testing.TB, si, sr *Session) {
	t.Helper()
	if !bytes.Equal(si.SID, sr.SID) || len(si.SID) != 16 || !bytes.Equal(si.SessionSecret, sr.SessionSecret) || !bytes.Equal(si.H, sr.H) {
		t.Fatal("session values differ")
	}
	if !bytes.Equal(si.KI2R.Key(), sr.KI2R.Key()) || !bytes.Equal(si.KR2I.Key(), sr.KR2I.Key()) {
		t.Fatal("transport keys differ")
	}
}

func TestXKHandshake(t *testing.T) {
	w := newWorld(t)
	si, sr := w.handshake(t)
	checkSessions(t, si, sr)
	if si.Suite != SuiteXK || sr.Suite != SuiteXK {
		t.Fatal("suite")
	}
	if sr.Peer == nil || sr.Peer.DeviceID != w.iID.deviceID() || si.Peer.DeviceID != w.rID.deviceID() {
		t.Fatal("peer identities")
	}
	if want := []uint32{1, 3, 4, 5}; !equal32(si.Features, want) || !equal32(sr.Features, want) {
		t.Fatalf("features %v %v", si.Features, sr.Features)
	}
	if len(si.Services) != 3 || si.Services[2] != (ServiceChoice{3, 2}) {
		t.Fatalf("services %v", si.Services)
	}
	if !bytes.Equal(si.Observed, w.rc.Observed) {
		t.Fatal("observed address")
	}
}

func TestXXHandshakeWithExpectedID(t *testing.T) {
	w := newWorld(t)
	id := w.rID.deviceID()
	w.ic.Remote, w.ic.ExpectedDeviceID = nil, &id
	si, sr := w.handshake(t)
	checkSessions(t, si, sr)
	if si.Suite != SuiteXX || si.Peer.DeviceID != id {
		t.Fatal("XX with expected ID")
	}
}

func TestXXHandshakeBareAddress(t *testing.T) {
	w := newWorld(t)
	w.ic.Remote = nil
	si, sr := w.handshake(t)
	checkSessions(t, si, sr)
	if !si.Unverified {
		t.Fatal("bare-address XX must mark the session unverified")
	}
}

func TestGreaseIsIgnored(t *testing.T) {
	w := newWorld(t)
	w.ic.Grease = true
	w.ic.Rand = rand.Reader
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, err := ini.Start()
	if err != nil {
		t.Fatal(err)
	}
	h, _ := ParseHello(hello)
	g := 0
	for _, s := range h.Offered {
		if IsGrease(s) {
			g++
		}
	}
	if g != 1 || IsGrease(h.Chosen) {
		t.Fatalf("offered %v", h.Offered)
	}
	res := NewResponder(w.rc)
	rep := res.HandleFirst(testCB, hello, m1, w.now)
	m3, si, err := ini.HandleMsg2(rep.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.HandleFinish(m3); err != nil {
		t.Fatal(err)
	}
	for _, f := range si.Features {
		if IsGrease32(f) {
			t.Fatal("GREASE feature selected")
		}
	}
	for _, s := range si.Services {
		if IsGrease32(s.ID) {
			t.Fatal("GREASE service selected")
		}
	}
	p1 := ini.lastInit
	if !strictlyAscending(p1.Features) {
		t.Fatal("features not sorted with GREASE")
	}
}

func TestHsKeyRotation(t *testing.T) {
	w := newWorld(t)
	old := w.rID.hs
	fresh := newTestIdentity(t, 9).hs
	// The initiator holds the old card (current key = old); the responder rotated.
	w.rc.Static, w.rc.PrevStatic = fresh, old
	si, sr := w.handshake(t)
	checkSessions(t, si, sr)
	// Without the previous key the old card no longer works.
	w.rc.PrevStatic = nil
	ini, _ := NewInitiator(w.ic, testCB)
	hello, m1, _ := ini.Start()
	if rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, w.now); rep.Action != ActClose {
		t.Fatalf("old key accepted without prev_hs: %v", rep.Action)
	}
}

func TestResponderCardOnlyWhenNewer(t *testing.T) {
	w := newWorld(t)
	si, _ := w.handshake(t) // known_card_seq = card seq: no card in P2
	if si.ResponderCardSent {
		t.Fatal("card sent although the initiator has it")
	}
	w.rc.CardSeq = fixedTime + 5
	si, _ = w.handshake(t)
	if !si.ResponderCardSent {
		t.Fatal("newer card not sent")
	}
}

func TestEarlyData(t *testing.T) {
	w := newWorld(t)
	calls := 0
	w.rc.Early = func(req []byte) ([]byte, bool) {
		calls++
		return append([]byte("re:"), req...), true
	}
	w.ic.Early = []byte("FIND_NODE")
	si, sr := w.handshake(t)
	if si.EarlyStatus != EarlyProcessed || string(si.EarlyResponse) != "re:FIND_NODE" || calls != 1 {
		t.Fatalf("early: %d %q", si.EarlyStatus, si.EarlyResponse)
	}
	if string(sr.EarlyRequest) != "FIND_NODE" {
		t.Fatal("responder early request")
	}
	// The responder does not support early-data: ignored.
	w.rc.Features = []uint32{1, 3, 4, 6}
	si, _ = w.handshake(t)
	if si.EarlyStatus != EarlyIgnored || calls != 1 {
		t.Fatalf("early without feature: %d", si.EarlyStatus)
	}
}

func TestEarlyDataNotInXX(t *testing.T) {
	w := newWorld(t)
	w.ic.Remote = nil
	w.ic.Early = []byte("x")
	if _, err := NewInitiator(w.ic, testCB); err == nil {
		t.Fatal("early data allowed in XX")
	}
	w.ic.Early, w.ic.AccessSecret = nil, bytes.Repeat([]byte{1}, 16)
	if _, err := NewInitiator(w.ic, testCB); err == nil {
		t.Fatal("access allowed in XX")
	}
}

func TestPuzzleRoundTrip(t *testing.T) {
	w := newWorld(t)
	w.rc.Puzzle = &PuzzleConfig{Secret: bytes.Repeat([]byte{7}, 32), Difficulty: func() uint8 { return 8 }}
	ini, _ := NewInitiator(w.ic, testCB)
	res := NewResponder(w.rc)
	hello, m1, _ := ini.Start()
	rep := res.HandleFirst(testCB, hello, m1, w.now)
	if rep.Action != ActSend || rep.FrameType != FrameRetry {
		t.Fatalf("want HS_RETRY, got %v %v", rep.Action, rep.FrameType)
	}
	hello2, m1b, err := ini.HandleRetry(rep.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(m1[:32], m1b[:32]) {
		t.Fatal("retry must use fresh ephemerals")
	}
	// The cookie of the previous minute is still accepted.
	rep = res.HandleFirst(testCB, hello2, m1b, w.now.Add(60*time.Second))
	if rep.Action != ActSend || rep.FrameType != FrameMsg {
		t.Fatalf("solution refused: %v %v", rep.Action, rep.Err)
	}
	m3, si, err := ini.HandleMsg2(rep.Payload)
	if err != nil {
		t.Fatal(err)
	}
	sr, err := res.HandleFinish(m3)
	if err != nil {
		t.Fatal(err)
	}
	checkSessions(t, si, sr)
}

func equal32(a, b []uint32) bool {
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

func isReject(err error, reason uint32) bool {
	var r *Reject
	return errors.As(err, &r) && r.Reason == reason
}

// peekP2 rebuilds the initiator of a flight from the same seeded randomness and returns the P2
// that a responder sent to (a replay of) that flight.
func (w *world) peekP2(hello, m1, m2 []byte) (*HsResp, error) {
	ic := w.ic
	ic.Rand = newSeeded(3)
	ini, err := NewInitiator(ic, testCB)
	if err != nil {
		return nil, err
	}
	ini.timeOverride = fixedTime
	h, m, err := ini.Start()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(h, hello) || !bytes.Equal(m, m1) {
		return nil, errors.New("flight not reproduced")
	}
	ini.HandleMsg2(m2)
	if ini.lastResp == nil {
		return nil, errors.New("message 2 not decrypted")
	}
	return ini.lastResp, nil
}
