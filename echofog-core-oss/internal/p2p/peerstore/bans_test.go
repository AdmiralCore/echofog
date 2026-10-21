package peerstore

import (
	"testing"
	"time"
)

func TestPeerStore_TimedBanSurvivesRestartAndRunsOut(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()

	ps := NewPeerStore(st, nil)
	_, short := randomPubKey(t)
	_, long := randomPubKey(t)
	_, forever := randomPubKey(t)
	if err := ps.BanPeerFor(short, 150*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	_ = ps.BanPeerFor(long, time.Hour)
	_ = ps.BanPeer(forever)
	// a shorter ban does not cut a longer one short
	_ = ps.BanPeerFor(long, time.Millisecond)
	_ = ps.BanPeerFor(forever, time.Millisecond)
	_ = ps.Close()

	ps = NewPeerStore(st, nil) // restart: bans come back from storage
	defer ps.Close()
	if !ps.IsBanned(short) || !ps.IsBanned(long) || !ps.IsBanned(forever) {
		t.Fatal("bans must survive a restart")
	}
	time.Sleep(200 * time.Millisecond)
	if ps.IsBanned(short) {
		t.Fatal("a timed ban must run out")
	}
	if !ps.IsBanned(long) || !ps.IsBanned(forever) {
		t.Fatal("the other bans still hold")
	}
	// the run-out ban is gone from storage too
	ps2 := NewPeerStore(st, nil)
	defer ps2.Close()
	if ps2.IsBanned(short) {
		t.Fatal("expired ban came back")
	}
}

func TestPeerStore_UnknownIDsAreNotRemembered(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()
	ps := NewPeerStore(st, nil)
	defer ps.Close()
	for i := 0; i < 500; i++ {
		_, id := randomPubKey(t)
		if ps.IsBanned(id) {
			t.Fatal("random peer banned")
		}
	}
	ps.bansMu.RLock()
	n := len(ps.banned)
	ps.bansMu.RUnlock()
	if n != 0 {
		t.Fatalf("lookups of free identities grew the ban cache to %d", n)
	}
}
