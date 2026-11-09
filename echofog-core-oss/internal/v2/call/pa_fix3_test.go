package call

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
)

// A peer with a session can push signed signals at the rate of its stream; the cheap per-device
// budget must refuse them before Parse and Ed25519 verify, and the reason must stay in the stats.
func TestSignalRateIsCheckedBeforeParsing(t *testing.T) {
	signer, peerSigner := testSigner(t, 50), testSigner(t, 51)
	delivered := 0
	s := newCallService(t, Config{
		Origin: signer.Public(), Sign: signFn(signer),
		MaxSignalsPerSecond: 2,
		OnSignal:            func(context.Context, Peer, *pb.CallSignal, []byte) { delivered++ },
	})
	peer := Peer{Device: deviceID(peerSigner.Public())}
	sender := deviceBytes(peerSigner.Public())
	for i := 0; i < 2; i++ {
		m := testPayload(ActionNack)
		m.Sender = sender
		m.MsgId = uint64(i + 1)
		raw, err := Encode(m, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Process(context.Background(), peer, raw); err != nil {
			t.Fatalf("signal %d: %v", i, err)
		}
	}
	// Garbage after the budget is refused as a rate refusal, not as malformed: it never reaches Parse.
	if err := s.Process(context.Background(), peer, []byte{0xff}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("garbage after the budget: %v, want ErrRateLimited", err)
	}
	if st := s.Stats(); st.SignalsRateLimited != 1 || delivered != 2 {
		t.Fatalf("stats %+v, delivered %d", st, delivered)
	}
}

// A device whose raw clock is minutes off still sends with the corrected clock (AD13): its join
// is not graded as stale by a JoinGuard that reads the corrected one.
func TestSelfSignalCarriesTheCorrectedClockLikeTheJoinGuard(t *testing.T) {
	aSigner, bSigner := testSigner(t, 52), testSigner(t, 53)
	aID, bID := deviceBytes(aSigner.Public()), deviceBytes(bSigner.Public())
	fixed := time.UnixMilli(1_800_000_000_000)
	got := make(chan *pb.CallSignal, 1)
	b := newCallService(t, Config{Origin: bSigner.Public(), Sign: signFn(bSigner),
		OnSignal: func(_ context.Context, _ Peer, m *pb.CallSignal, _ []byte) { got <- m }})
	a := newCallService(t, Config{Origin: aSigner.Public(), Sign: signFn(aSigner),
		Now:  func() time.Time { return fixed },
		Open: pairOpen(t, b, bID, aID)})
	m := testPayload(ActionJoin)
	m.Timestamp = time.Now().Add(-5 * time.Minute).UnixMilli() // the raw clock of a device that is off
	if err := a.Send(context.Background(), Peer{Device: deviceID(bSigner.Public())}, m); err != nil {
		t.Fatal(err)
	}
	var rec *pb.CallSignal
	select {
	case rec = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("join not delivered")
	}
	if rec.Timestamp != fixed.UnixMilli() {
		t.Fatalf("self signal carries %d, want the corrected clock %d", rec.Timestamp, fixed.UnixMilli())
	}
	g := NewJoinGuard(JoinGuardConfig{Now: func() time.Time { return fixed },
		After: func(time.Duration) <-chan time.Time { c := make(chan time.Time, 1); c <- fixed; return c }})
	if err := g.Admit(context.Background(), rec.CallId, deviceID(aSigner.Public()), rec.GetJoin(), rec.Timestamp); err != nil {
		t.Fatalf("the join looks stale next to the corrected clock: %v", err)
	}
}

// Our own datagrams can come back on a loop; they are not a foreign peer and must not spend the
// per-device budget (the same way JoinGuard accounts a device's requests apart from the others).
func TestSelfSignalsDoNotSpendTheForeignBudget(t *testing.T) {
	signer, peerSigner := testSigner(t, 54), testSigner(t, 55)
	delivered := 0
	s := newCallService(t, Config{Origin: signer.Public(), Sign: signFn(signer), MaxSignalsPerSecond: 1,
		OnSignal: func(context.Context, Peer, *pb.CallSignal, []byte) { delivered++ }})
	selfPeer := Peer{Device: deviceID(signer.Public())}
	for i := 0; i < 5; i++ {
		m := testPayload(ActionPing)
		m.Sender = deviceBytes(signer.Public())
		m.MsgId = uint64(100 + i)
		raw, err := Encode(m, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Process(context.Background(), selfPeer, raw); err != nil {
			t.Fatalf("self signal %d: %v", i, err)
		}
	}
	if st := s.Stats(); st.SignalsSelf != 5 {
		t.Fatalf("self signals counted as foreign: %+v", st)
	}
	m := testPayload(ActionPing)
	m.Sender = deviceBytes(peerSigner.Public())
	m.MsgId = 200
	raw, err := Encode(m, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Process(context.Background(), Peer{Device: deviceID(peerSigner.Public())}, raw); err != nil {
		t.Fatalf("self traffic spent the foreign budget: %v", err)
	}
	if delivered != 6 {
		t.Fatalf("delivered %d signals, want 6", delivered)
	}
}
