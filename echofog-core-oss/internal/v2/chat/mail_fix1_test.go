package chat

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// ---- announcements are bounded ----------------------------------------------------------------------

func TestAnnouncementLimits(t *testing.T) {
	w := newWorld(t)
	dirB := NewMemMailDirectory()
	known := map[[32]byte]bool{}
	w.tune = func(c *ServiceConfig) {
		c.MailDir = dirB
		c.IsContact = func(n [32]byte) bool { return known[n] }
	}
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "hello")
	w.tick(a)
	announce := func(notAfter time.Time) {
		_ = a.svc.AnnounceQueue(b.self.Node, mailbox.QueueRef{Relay: [32]byte{7}, Queue: [16]byte{8}, Expires: notAfter}, nil)
		w.tick(a)
	}
	// not a contact: ignored
	announce(w.clock.Now().Add(9 * 24 * time.Hour))
	if _, ok := dirB.Get(a.self.Node); ok {
		t.Fatal("a stranger's announcement was kept")
	}
	known[a.self.Node] = true
	// an end a hundred years away, or beyond 30 days, is ignored
	announce(w.clock.Now().Add(100 * 365 * 24 * time.Hour))
	announce(w.clock.Now().Add(31 * 24 * time.Hour))
	if _, ok := dirB.Get(a.self.Node); ok {
		t.Fatal("an announcement that ends too late was kept")
	}
	announce(w.clock.Now().Add(9 * 24 * time.Hour))
	if _, ok := dirB.Get(a.self.Node); !ok {
		t.Fatal("a contact's announcement was not kept")
	}
}

func TestMemMailDirectoryIsBounded(t *testing.T) {
	d := NewMemMailDirectory()
	d.Max = 3
	base := time.Unix(2_000_000_000, 0)
	for i := 0; i < 6; i++ {
		_ = d.Put([32]byte{byte(i + 1)}, target(byte(i), base.Add(time.Duration(i)*time.Hour)))
	}
	if d.Len() != 3 {
		t.Fatalf("%d targets, want 3", d.Len())
	}
	if _, ok := d.Get([32]byte{6}); !ok {
		t.Fatal("the newest target was evicted")
	}
	if _, ok := d.Get([32]byte{1}); ok {
		t.Fatal("the oldest target was kept")
	}
	d.Prune(base.Add(4*time.Hour + time.Minute)) // targets 0..4 have ended; 5 is the last
	if d.Len() != 1 {
		t.Fatalf("%d targets after Prune, want 1", d.Len())
	}
}

func TestKVMailDirectoryIsBoundedAndExpires(t *testing.T) {
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Destroy)
	st, err := store.Open(store.Options{Dir: filepath.Join(t.TempDir(), "v2"), Key: tw.Account.StorageKey()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ids, err := store.NewIDs(tw.Account.StorageKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ids.Close)
	d := &KVMailDirectory{KV: st.Store("v2/chat/mail"), IDs: ids, Max: 3}
	base := time.Unix(2_000_000_000, 0)
	for i := 0; i < 6; i++ {
		if err := d.Put([32]byte{byte(i + 1)}, target(byte(i), base.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	if d.Len() != 3 {
		t.Fatalf("%d targets, want 3", d.Len())
	}
	// the store holds exactly the counted records: the target and its index entry each
	entries, _ := st.Store("v2/chat/mail").Scan(nil, 1000)
	if len(entries) != 6 {
		t.Fatalf("%d records for 3 targets", len(entries))
	}
	// a replaced target does not count twice
	_ = d.Put([32]byte{6}, target(9, base.Add(48*time.Hour)))
	if d.Len() != 3 {
		t.Fatalf("%d targets after a replacement", d.Len())
	}
	d.Prune(base.Add(100 * time.Hour))
	entries, _ = st.Store("v2/chat/mail").Scan(nil, 1000)
	if d.Len() != 0 || len(entries) != 0 {
		t.Fatalf("after expiry: %d targets, %d records", d.Len(), len(entries))
	}
}
