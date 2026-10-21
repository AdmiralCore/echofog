package peerstore

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"google.golang.org/protobuf/proto"
)

// putEntry writes an entry as it would be after some time, bypassing SavePeer's clock.
func putEntry(t *testing.T, ps *PeerStore, e *storage.PeerStoreEntry) {
	t.Helper()
	if err := ps.writeEntry(e); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryIsFreshestFirst(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()
	ps := NewPeerStore(st, nil)
	defer ps.Close()

	now := time.Now()
	var want []string
	for i := 0; i < 20; i++ {
		pub, _ := randomPubKey(t)
		addr := "203.0.113.1:" + string(rune('a'+i))
		putEntry(t, ps, &storage.PeerStoreEntry{PubKey: pub[:], LastKnownAddr: addr, LastSeen: uint64(now.Add(-time.Duration(i) * time.Hour).UnixNano())})
		want = append(want, addr)
	}
	got := ps.GetHistoryConnected(5)
	if len(got) != 5 {
		t.Fatalf("got %d entries, want 5", len(got))
	}
	for i, e := range got {
		if e.LastKnownAddr != want[i] {
			t.Fatalf("entry %d is %s, want %s: the history must be ordered by when peers were last seen, not by blinded key", i, e.LastKnownAddr, want[i])
		}
	}
}

func TestDialFailuresBackOffThenForget(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()
	ps := NewPeerStore(st, nil)
	defer ps.Close()

	pub, id := randomPubKey(t)
	if err := ps.SavePeer(pub, "203.0.113.9:4242", 100); err != nil {
		t.Fatal(err)
	}
	ps.RecordDialFailure(id)
	e, err := ps.GetPeer(id)
	if err != nil || e.FailCount != 1 || e.LastFailed == 0 {
		t.Fatalf("failure not recorded: %+v %v", e, err)
	}
	if DialDue(e, time.Now()) {
		t.Fatal("a peer that just failed is not tried again at once")
	}
	if !DialDue(e, time.Now().Add(16*time.Second)) {
		t.Fatal("after one failure the retry is due in 15 seconds")
	}

	// a successful connection clears the count
	if err := ps.SavePeer(pub, "203.0.113.9:4242", 100); err != nil {
		t.Fatal(err)
	}
	if e, _ := ps.GetPeer(id); e.FailCount != 0 {
		t.Fatal("a connection must clear the failures")
	}

	for i := 0; i < MaxDialFailures; i++ {
		ps.RecordDialFailure(id)
	}
	if _, err := ps.GetPeer(id); err == nil {
		t.Fatalf("a peer that failed %d times in a row must be forgotten", MaxDialFailures)
	}
}

func TestDialDueCapsTheWait(t *testing.T) {
	e := &storage.PeerStoreEntry{FailCount: 7, LastFailed: uint64(time.Now().UnixNano())}
	if DialDue(e, time.Now().Add(15*time.Second<<5)) {
		t.Fatal("the wait doubles per failure")
	}
	if !DialDue(e, time.Now().Add(15*time.Second<<6+time.Second)) {
		t.Fatal("seven failures wait 15s<<6")
	}
	e.FailCount = 30
	if !DialDue(e, time.Now().Add(retryMax+time.Second)) {
		t.Fatal("the wait never exceeds retryMax")
	}
}

func TestPruneForgetsOldAndFailingPeers(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()
	ps := NewPeerStore(st, nil)
	defer ps.Close()

	now := time.Now()
	freshPub, freshID := randomPubKey(t)
	oldPub, oldID := randomPubKey(t)
	failPub, failID := randomPubKey(t)
	putEntry(t, ps, &storage.PeerStoreEntry{PubKey: freshPub[:], LastKnownAddr: "203.0.113.1:1", LastSeen: uint64(now.Add(-24 * time.Hour).UnixNano())})
	putEntry(t, ps, &storage.PeerStoreEntry{PubKey: oldPub[:], LastKnownAddr: "203.0.113.2:1", LastSeen: uint64(now.Add(-31 * 24 * time.Hour).UnixNano())})
	putEntry(t, ps, &storage.PeerStoreEntry{PubKey: failPub[:], LastKnownAddr: "203.0.113.3:1", LastSeen: uint64(now.UnixNano()), FailCount: MaxDialFailures})

	if n := ps.Prune(now); n != 2 {
		t.Fatalf("pruned %d, want 2", n)
	}
	if _, err := ps.GetPeer(freshID); err != nil {
		t.Fatal("a peer seen yesterday is kept")
	}
	for _, id := range [][32]byte{oldID, failID} {
		if _, err := ps.GetPeer(id); err == nil {
			t.Fatal("an old or failing peer survived the prune")
		}
	}
}

func TestSaveDiscoveredKeepsWhatWeKnow(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()
	ps := NewPeerStore(st, nil)
	defer ps.Close()

	pub, id := randomPubKey(t)
	seen := uint64(time.Now().Add(-48 * time.Hour).UnixNano())
	putEntry(t, ps, &storage.PeerStoreEntry{PubKey: pub[:], LastKnownAddr: "203.0.113.5:443", LastSeen: seen, TrustScore: 100})
	if ps.SaveDiscovered(pub, "198.51.100.1:443") {
		t.Fatal("a known peer is not replaced by what another node says")
	}
	e, _ := ps.GetPeer(id)
	if e.LastKnownAddr != "203.0.113.5:443" || e.LastSeen != seen {
		t.Fatalf("entry changed: %+v", e)
	}

	newPub, newID := randomPubKey(t)
	if !ps.SaveDiscovered(newPub, "198.51.100.2:443") {
		t.Fatal("a new peer is kept")
	}
	if e, err := ps.GetPeer(newID); err != nil || e.TrustScore >= 100 {
		t.Fatalf("discovered peer: %+v %v", e, err)
	}
	// garbage in the store is skipped, not returned
	_ = st.Set([]byte(savedPeersPrefix+"junk"), []byte{0xff, 0x01})
	raw, _ := proto.Marshal(&storage.PeerStoreEntry{PubKey: []byte{1, 2, 3}})
	_ = st.Set([]byte(savedPeersPrefix+"short"), raw)
	if n := len(ps.GetHistoryConnected(0)); n != 2 {
		t.Fatalf("history has %d entries, want the 2 valid ones", n)
	}
}

func TestSaveDiscoveredStopsAtTheCap(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()
	ps := NewPeerStore(st, nil)
	defer ps.Close()

	ps.stored, ps.countAt = MaxStored-1, time.Now()
	pub, _ := randomPubKey(t)
	if !ps.SaveDiscovered(pub, "203.0.113.9:1") {
		t.Fatal("room for one more")
	}
	pub2, _ := randomPubKey(t)
	if ps.SaveDiscovered(pub2, "203.0.113.10:1") {
		t.Fatal("a full store takes in no more peers it only heard of")
	}
	pub3, id3 := randomPubKey(t)
	if err := ps.SavePeer(pub3, "203.0.113.11:1", 50); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.GetPeer(id3); err != nil {
		t.Fatal("a peer we connected to is saved whatever the cap")
	}
	ps.Prune(time.Now())
	pub4, _ := randomPubKey(t)
	if !ps.SaveDiscovered(pub4, "203.0.113.12:1") {
		t.Fatal("after a prune the store is counted again and has room")
	}
}
