package session_test

import (
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/session"
	"github.com/echofog/echofog-core/internal/v2/store"
)

func testStorageKey(t *testing.T, seed byte) *secret.Value {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	v := secret.New(b)
	t.Cleanup(v.Destroy)
	return v
}

func openStoreAt(t *testing.T, dir string, key *secret.Value) *store.Store {
	t.Helper()
	st, err := store.Open(store.Options{Dir: dir, Key: key})
	if err != nil {
		t.Fatalf("store.Open(%s): %v", dir, err)
	}
	return st
}

// TestKeyBindingsPersistAcrossRestart: hs↔device bindings live in the store (handshake.md §7,
// S15) and a conflicting binding is abuse against the device that supplied the card.
func TestKeyBindingsPersistAcrossRestart(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	hs := b.device.Handshake().Public().Key()

	dir := t.TempDir()
	key := testStorageKey(t, 1)

	st := openStoreAt(t, dir, key)
	abuse, err := session.NewAbuseLog(st.Store("v2/session/abuse"))
	if err != nil {
		t.Fatalf("NewAbuseLog: %v", err)
	}
	bindings, err := session.NewKeyBindings(st.Store("v2/session/bindings"), abuse)
	if err != nil {
		t.Fatalf("NewKeyBindings: %v", err)
	}
	if err := bindings.Bind(hs, b.device.ID()); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	st2 := openStoreAt(t, dir, key)
	t.Cleanup(func() { _ = st2.Close() })
	abuse2, err := session.NewAbuseLog(st2.Store("v2/session/abuse"))
	if err != nil {
		t.Fatalf("NewAbuseLog after restart: %v", err)
	}
	bindings2, err := session.NewKeyBindings(st2.Store("v2/session/bindings"), abuse2)
	if err != nil {
		t.Fatalf("NewKeyBindings after restart: %v", err)
	}
	got, ok, err := bindings2.Lookup(hs)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok || got != b.device.ID() {
		t.Fatalf("binding after restart = %x %v, want %x", got, ok, b.device.ID())
	}

	// A different device claiming the same hs key is a conflict and counts as abuse against it.
	if err := bindings2.Bind(hs, a.device.ID()); !errors.Is(err, handshake.ErrKeyBinding) {
		t.Fatalf("conflicting Bind = %v, want ErrKeyBinding", err)
	}
	count, err := abuse2.Count(a.device.ID())
	if err != nil {
		t.Fatalf("Abuse Count: %v", err)
	}
	if count != 1 {
		t.Fatalf("abuse count for the source = %d, want 1", count)
	}

	// The count survives a restart as well.
	if err := st2.Close(); err != nil {
		t.Fatalf("store.Close 2: %v", err)
	}
	st3 := openStoreAt(t, dir, key)
	t.Cleanup(func() { _ = st3.Close() })
	abuse3, err := session.NewAbuseLog(st3.Store("v2/session/abuse"))
	if err != nil {
		t.Fatalf("NewAbuseLog after second restart: %v", err)
	}
	if count, err := abuse3.Count(a.device.ID()); err != nil || count != 1 {
		t.Fatalf("abuse count after restart = %d (%v), want 1", count, err)
	}
}

// TestCardBookPersistsAcrossRestart: the card book is in the store, so cards and the
// misbehaving marker survive a restart.
func TestCardBookPersistsAcrossRestart(t *testing.T) {
	now := time.Now()
	b := newTestNode(t)
	suites := []uint16{handshake.SuiteXK, handshake.SuiteXX}
	notAfter := uint64(now.Add(24 * time.Hour).Unix())
	first := b.card(t, objects.ScopePeer, suites, 50, notAfter)
	c1 := verifyCard(t, first, now)

	dir := t.TempDir()
	key := testStorageKey(t, 2)

	st := openStoreAt(t, dir, key)
	book, err := session.OpenCardBook(st.Store("v2/session/cards"))
	if err != nil {
		t.Fatalf("OpenCardBook: %v", err)
	}
	if _, err := book.Add(c1); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	// A second object with the same signer, label and seq is equivocation.
	second := b.card(t, objects.ScopePeer, suites, 50, uint64(now.Add(48*time.Hour).Unix()))
	c2 := verifyCard(t, second, now)
	if _, err := book.Add(c2); !errors.Is(err, objects.ErrEquivocation) {
		t.Fatalf("Add second = %v, want ErrEquivocation", err)
	}
	if !book.Misbehaving(b.device.ID()) {
		t.Fatal("the signer is not marked misbehaving before the restart")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	st2 := openStoreAt(t, dir, key)
	t.Cleanup(func() { _ = st2.Close() })
	book2, err := session.OpenCardBook(st2.Store("v2/session/cards"))
	if err != nil {
		t.Fatalf("OpenCardBook after restart: %v", err)
	}
	c := book2.Get(b.device.ID(), objects.ScopePeer, nil)
	if c == nil || c.Env.Seq != 50 {
		t.Fatalf("card after restart = %+v, want seq 50", c)
	}
	if !book2.Misbehaving(b.device.ID()) {
		t.Fatal("the misbehaving marker did not survive the restart")
	}
}
