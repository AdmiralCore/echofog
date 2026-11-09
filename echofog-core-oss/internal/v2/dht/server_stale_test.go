package dht

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// TestStaleAcrossAdjacentDayKeyIsRefused reproduces ДР-1: a node may hold a record under the
// current-day key but not under the previous one (it became responsible later, or the older copy
// expired). An old signed record with a lower seq must then be refused as stale, not stored under
// the missing adjacent key; otherwise a reader in the first two hours of the day can accept it.
func TestStaleAcrossAdjacentDayKeyIsRefused(t *testing.T) {
	now := srvNow
	sg := seededSigner(t, 4900)
	ks := newKeySet()
	ks.add(dayKey(objects.LabelMailboxHint, sg, srvNow))
	s := newServer(t, func(c *ServerConfig) { ks.config(c); c.Now = func() time.Time { return now } })
	p := testPeer("192.0.2.0/24")

	if r := s.StoreAt(p, hintRecord(t, sg, 2, expiry(time.Hour)), now); r.Status != StatusStored {
		t.Fatalf("seq 2: %+v", r)
	}
	previous := ks.add(dayKey(objects.LabelMailboxHint, sg, srvNow.Add(-24*time.Hour)))

	if r := s.StoreAt(p, hintRecord(t, sg, 1, expiry(time.Hour)), now.Add(time.Second)); r.Status != StatusStale {
		t.Fatalf("lower seq under a missing adjacent key: %+v, want stale", r)
	}
	if got, _ := s.FindValue(previous); got != nil {
		t.Fatal("a stale record was stored under the previous-day key")
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want the one newer record", s.Len())
	}
	s.checkInvariants(t)
}
