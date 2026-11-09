package call

import (
	"context"
	"errors"
	"testing"

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
