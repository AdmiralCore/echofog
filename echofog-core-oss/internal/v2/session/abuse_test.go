package session_test

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/session"
)

// TestAbuseSnapshotListsSources: the status view needs every stored counter, not only the count
// of one source (T0.3 `efv2 status`).
func TestAbuseSnapshotListsSources(t *testing.T) {
	log, err := session.NewAbuseLog(newMemKV())
	if err != nil {
		t.Fatal(err)
	}
	var one, two [32]byte
	one[0], two[0] = 1, 2
	for i := 0; i < 3; i++ {
		if err := log.Report(one); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Report(two); err != nil {
		t.Fatal(err)
	}

	snap, err := log.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d counters, want 2: %+v", len(snap), snap)
	}
	got := map[[32]byte]uint64{}
	for _, c := range snap {
		got[c.Source] = c.Count
	}
	if got[one] != 3 || got[two] != 1 {
		t.Fatalf("snapshot %+v, want %x=3 %x=1", snap, one, two)
	}
}
