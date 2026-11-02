package convlog

import (
	"strings"
	"testing"
)

// One Want brings a bounded answer (the rest on asking again), and a peer that keeps asking gets
// answers only at a limited rate: a ~50-byte Want used to pull up to 2 MiB, 40 times in 10 s.
func TestWantAnswerIsBoundedAndRateLimited(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	la := replica(t, a)
	ca := DM(a.id, b.id)
	big := strings.Repeat("x", MaxTextLen-64)
	for i := 0; i < 200; i++ { // ~3 MiB of history
		mustF(t)(la.AppendMessage(ca, big, 0, "", EventID{}, EventID{}))
	}
	want := &Sync{M: &Sync_Want{Want: &Want{Conv: ca.ID[:]}}} // has nothing

	answer := la.HandleSync(b.id, want, nil)
	total := 0
	for _, s := range answer {
		for _, e := range s.GetEvents().GetEvents() {
			total += len(e)
		}
	}
	if total == 0 || total > 256<<10 {
		t.Fatalf("one Want brought %d bytes", total)
	}
	if !answer[len(answer)-1].GetEvents().GetMore() {
		t.Fatal("a cut answer must say there is more")
	}

	answered := 1
	for i := 0; i < 40; i++ {
		if len(la.HandleSync(b.id, want, nil)) > 0 {
			answered++
		}
	}
	if answered > 20 {
		t.Fatalf("%d of 41 Wants in a row were answered", answered)
	}
}
