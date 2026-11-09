package sigbridge

import (
	"testing"

	"github.com/echofog/echofog-core/internal/services/call/signaling"
	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
)

// A viewer's path report travels only on the lossy copy (feedback.go sends no reliable one), so the
// bridge must not swallow it: without it the broadcaster never learns the path is full and the rate
// of a 4K60 stream never comes down.
func TestStreamFeedbackIsSentOnTheLossyCopy(t *testing.T) {
	r := newRig(t, false)
	fb := signaling.StreamFeedbackPayload{StreamID: "s1", Reports: []signaling.ViewerFeedback{
		{Viewer: pid(3), Level: 3, Kbps: 900, Loss: 0.2, DelayMs: 60},
	}}
	if err := r.b.SendSignal(pid(2), true, v1env(t, r.self, signaling.ActionStreamFeedback, fb)); err != nil {
		t.Fatal(err)
	}
	if len(r.svc.sent) != 1 || len(r.svc.fwd) != 0 {
		t.Fatalf("path feedback did not reach the service: sent %d forwarded %d", len(r.svc.sent), len(r.svc.fwd))
	}
	if got := r.svc.sent[0]; got.Action != pb.Action_ACTION_STREAM_FEEDBACK || got.GetStreamFeedback().GetStreamId() != "s1" {
		t.Fatalf("sent %+v", got)
	}

	// A NACK already goes reliably as well; its lossy copy must not become a second reliable send.
	if err := r.b.SendSignal(pid(2), true, v1env(t, r.self, signaling.ActionNack, signaling.NackPayload{StreamID: "s1"})); err != nil {
		t.Fatal(err)
	}
	if len(r.svc.sent) != 1 {
		t.Fatalf("a lossy NACK copy was sent again: %d signals", len(r.svc.sent))
	}
}
