package keymanager

import (
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func countPrefix(st *mockStorage, prefix string) int {
	n := 0
	for k := range st.data {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

func TestJITBundlePrekeyIsUsableAndCounted(t *testing.T) {
	acc, st := testAccount(t)
	b, err := acc.CreateJITBundle()
	if err != nil {
		t.Fatal(err)
	}
	if len(b.OneTimeKeys) != 1 || acc.StoredJITOPKs() != 1 || countPrefix(st, jitOPKPrefix) != 1 {
		t.Fatal("one handed-out prekey stored and counted")
	}
	if _, err := acc.ConsumeOPK(b.OneTimeKeys[0]); err != nil {
		t.Fatalf("the prekey of a JIT bundle must open: %v", err)
	}
	if acc.StoredJITOPKs() != 0 || countPrefix(st, jitOPKPrefix) != 0 {
		t.Fatal("a used prekey is gone and no longer counted")
	}
}

func storeAgedJIT(t *testing.T, acc *AccountManager, created time.Time) [32]byte {
	t.Helper()
	var pub [32]byte
	priv := make([]byte, 32)
	_, _ = rand.Read(pub[:])
	_, _ = rand.Read(priv)
	name := append([]byte(jitOPKPrefix), pub[:]...)
	sealed, err := acc.seal(name, timedSecret(created, priv))
	if err != nil {
		t.Fatal(err)
	}
	if err := acc.storage.Set(name, sealed); err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestUnusedHandedOutPrekeysAreSwept(t *testing.T) {
	acc, _ := testAccount(t)
	old := storeAgedJIT(t, acc, time.Now().Add(-JITOPKRetention-time.Hour))
	fresh := storeAgedJIT(t, acc, time.Now().Add(-time.Hour))
	published, err := acc.CreateBundleWithOPKs(2)
	if err != nil {
		t.Fatal(err)
	}

	if left := acc.SweepJITOPKs(); left != 1 {
		t.Fatalf("%d handed-out prekeys left, want 1", left)
	}
	if _, err := acc.OpenOPK(old); err == nil {
		t.Fatal("a prekey handed out long ago and never used must go")
	}
	if _, err := acc.OpenOPK(fresh); err != nil {
		t.Fatal("a recent one stays")
	}
	for _, pub := range published.OneTimeKeys {
		if _, err := acc.OpenOPK(pub); err != nil {
			t.Fatal("the sweep must leave the prekeys of published bundles alone")
		}
	}
}

func TestHandedOutPrekeysAreCapped(t *testing.T) {
	acc, st := testAccount(t)
	published, err := acc.CreateBundleWithOPKs(2)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-2 * time.Hour)
	first := storeAgedJIT(t, acc, start)
	for i := 1; i < MaxJITOPKs; i++ {
		storeAgedJIT(t, acc, start.Add(time.Duration(i)*time.Second))
	}
	if _, err := acc.CreateJITBundle(); err != nil {
		t.Fatal(err)
	}
	if n := countPrefix(st, jitOPKPrefix); n > MaxJITOPKs || n != acc.StoredJITOPKs() {
		t.Fatalf("%d handed-out prekeys stored (counted %d), cap %d", n, acc.StoredJITOPKs(), MaxJITOPKs)
	}
	if _, err := acc.OpenOPK(first); err == nil {
		t.Fatal("the oldest handed-out prekey goes first")
	}
	for _, pub := range published.OneTimeKeys {
		if _, err := acc.OpenOPK(pub); err != nil {
			t.Fatal("published prekeys are never evicted for handed-out ones")
		}
	}
}
