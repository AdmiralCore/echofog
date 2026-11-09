package bootstrap_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// memKV is a store.KV for the tests: the profile store is not needed to prove the list rules.
type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemKV() *memKV { return &memKV{m: map[string][]byte{}} }

func (k *memKV) Get(key []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[string(key)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (k *memKV) Put(key, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[string(key)] = append([]byte(nil), value...)
	return nil
}

func (k *memKV) PutTTL(key, value []byte, _ time.Duration) error { return k.Put(key, value) }

func (k *memKV) Delete(key []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, string(key))
	return nil
}

func (k *memKV) Has(key []byte) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.m[string(key)]
	return ok, nil
}

func (k *memKV) Scan(prefix []byte, limit int) ([]store.Entry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []store.Entry
	for key, v := range k.m {
		if strings.HasPrefix(key, string(prefix)) && len(out) < limit {
			out = append(out, store.Entry{Key: []byte(key), Value: append([]byte(nil), v...)})
		}
	}
	return out, nil
}

func (k *memKV) ScanFrom(prefix, start []byte, limit int) ([]store.Entry, error) {
	return k.Scan(prefix, limit)
}

var _ store.KV = (*memKV)(nil)

func testSigner(t *testing.T, seed byte) *keys.Signer {
	t.Helper()
	s, err := keys.NewEd25519Signer(bytes.Repeat([]byte{seed}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const listA = "noise3://192.0.2.10:4430?mask=" + hexM
const listB = "noise3://192.0.2.11:4430?mask=" + hexM + "&device=" + hexA

func TestSignedListRoundTrip(t *testing.T) {
	s := testSigner(t, 7)
	raw, err := bootstrap.SealList(s, 12, uint64(testNow.Add(30*24*time.Hour).Unix()), listA+"\n"+listB+"\n")
	if err != nil {
		t.Fatal(err)
	}
	l, err := bootstrap.OpenList(raw, []keys.AKey{s.Public()}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if l.Seq != 12 || len(l.Entries) != 2 || l.Signer.Text() != s.Public().Text() {
		t.Fatalf("list %+v", l)
	}
	if l.Entries[1].Kind != bootstrap.KindDevice || l.Entries[1].Device[0] != 1 {
		t.Fatalf("device entry lost: %+v", l.Entries[1])
	}

	// a changed byte breaks the signature
	bad := append([]byte(nil), raw...)
	bad[40] ^= 0xff
	if _, err := bootstrap.OpenList(bad, []keys.AKey{s.Public()}, testNow); !errors.Is(err, bootstrap.ErrListSignature) {
		t.Fatalf("forged list: %v", err)
	}
	// a key that is not a release key is not a trust root
	if _, err := bootstrap.OpenList(raw, []keys.AKey{testSigner(t, 8).Public()}, testNow); !errors.Is(err, bootstrap.ErrListSignature) {
		t.Fatalf("foreign key: %v", err)
	}
	// the term is enforced
	if _, err := bootstrap.OpenList(raw, []keys.AKey{s.Public()}, testNow.Add(31*24*time.Hour)); !errors.Is(err, bootstrap.ErrListExpired) {
		t.Fatalf("expired list: %v", err)
	}
	// bad entries fail closed
	badBody, err := bootstrap.SealList(s, 13, uint64(testNow.Add(24*time.Hour).Unix()), "hello\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.OpenList(badBody, []keys.AKey{s.Public()}, testNow); !errors.Is(err, bootstrap.ErrList) {
		t.Fatalf("bad entry accepted: %v", err)
	}
	if _, err := bootstrap.SealList(s, 0, 1, listA); err == nil {
		t.Fatal("seq 0 sealed")
	}
}

func newManager(t *testing.T, kv store.KV, custom ...string) (*bootstrap.Manager, *keys.Signer) {
	t.Helper()
	s := testSigner(t, 7)
	builtin, err := bootstrap.SealList(s, 10, uint64(testNow.Add(30*24*time.Hour).Unix()), listA+"\n"+listB+"\n")
	if err != nil {
		t.Fatal(err)
	}
	m := bootstrap.OpenManager(bootstrap.ManagerConfig{
		Builtin: builtin, Keys: []keys.AKey{s.Public()}, Custom: custom, KV: kv,
		Now: func() time.Time { return testNow }, SaveCustom: func([]string) error { return nil },
	})
	if err := m.Err(); err != nil {
		t.Fatal(err)
	}
	return m, s
}

func TestManagerBuiltinToggleAndCustom(t *testing.T) {
	kv := newMemKV()
	m, _ := newManager(t, kv)
	if got := m.Builtin(); len(got) != 2 || !got[0].Builtin || got[0].Index != 1 || !got[0].Enabled {
		t.Fatalf("builtin %+v", got)
	}
	if len(m.Entries()) != 2 {
		t.Fatalf("entries %d", len(m.Entries()))
	}
	if err := m.SetBuiltinEnabled(2, false); err != nil {
		t.Fatal(err)
	}
	if got := m.Builtin(); got[1].Enabled {
		t.Fatal("the disabled entry is still enabled")
	}
	for _, e := range m.Entries() {
		if e.Index == 2 {
			t.Fatal("a disabled built-in entry is dialed")
		}
	}
	// the choice survives a restart
	m2 := bootstrap.OpenManager(bootstrap.ManagerConfig{Builtin: mustBuiltin(t), Keys: []keys.AKey{testSigner(t, 7).Public()},
		KV: kv, Now: func() time.Time { return testNow }})
	if got := m2.Builtin(); got[1].Enabled {
		t.Fatal("the disabled choice was lost")
	}

	if _, err := m.AddCustom("reality2-tcp://198.51.100.7:443?mask=" + hexM + "&sni=www.example.org&device=" + hexA); err != nil {
		t.Fatal(err)
	}
	if len(m.Custom()) != 1 || len(m.Entries()) != 2 {
		t.Fatalf("custom %d entries %d", len(m.Custom()), len(m.Entries()))
	}
	if _, err := m.AddCustom("not a list line"); err == nil {
		t.Fatal("a bad custom entry was accepted")
	}
	if err := m.RemoveCustom("reality2-tcp://198.51.100.7:443?mask=" + hexM + "&sni=www.example.org&device=" + hexA); err != nil {
		t.Fatal(err)
	}
	if len(m.Custom()) != 0 {
		t.Fatal("custom entry not removed")
	}
}

func mustBuiltin(t *testing.T) []byte {
	t.Helper()
	s := testSigner(t, 7)
	raw, err := bootstrap.SealList(s, 10, uint64(testNow.Add(30*24*time.Hour).Unix()), listA+"\n"+listB+"\n")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestManagerCustomPersists(t *testing.T) {
	var saved []string
	m := bootstrap.OpenManager(bootstrap.ManagerConfig{Custom: nil, Now: func() time.Time { return testNow },
		SaveCustom: func(lines []string) error { saved = append([]string(nil), lines...); return nil }})
	if _, err := m.AddCustom(listA); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 {
		t.Fatalf("the save hook got %d lines", len(saved))
	}
	m2 := bootstrap.OpenManager(bootstrap.ManagerConfig{Custom: saved, Now: func() time.Time { return testNow }})
	if got := m2.Custom(); len(got) != 1 || got[0].Entry.Line != listA {
		t.Fatalf("custom entry lost on restart: %+v", got)
	}
	if err := m.RemoveCustom(listA); err != nil || len(saved) != 0 {
		t.Fatalf("remove: %v, saved %d", err, len(saved))
	}
}

func TestManagerEmptyBuiltinOnSourceBuild(t *testing.T) {
	m := bootstrap.OpenManager(bootstrap.ManagerConfig{Now: func() time.Time { return testNow }})
	if len(m.Builtin()) != 0 || len(m.Entries()) != 0 {
		t.Fatalf("a build without a list has entries: %+v", m.Entries())
	}
	if _, err := m.AddCustom(listA); err != nil {
		t.Fatal(err)
	}
	if len(m.Entries()) != 1 {
		t.Fatal("the user's entry is not used")
	}
}

func TestManagerOfferRollbackForgeryExpiry(t *testing.T) {
	kv := newMemKV()
	m, s := newManager(t, kv)
	offer := func(seq uint64, text string, notAfter time.Time, signer *keys.Signer) error {
		raw, err := bootstrap.SealList(signer, seq, uint64(notAfter.Unix()), text)
		if err != nil {
			t.Fatal(err)
		}
		return m.Offer(raw)
	}
	line := "reality2-quic://198.51.100.9:443?mask=" + hexM + "&sni=www.example.org&device=" + hexA
	if err := offer(11, line+"\n", testNow.Add(24*time.Hour), s); err != nil {
		t.Fatal(err)
	}
	if m.Seq() != 11 || len(m.Entries()) != 1 {
		t.Fatalf("update not applied: seq %d entries %d", m.Seq(), len(m.Entries()))
	}
	if err := offer(10, line+"\n", testNow.Add(24*time.Hour), s); !errors.Is(err, bootstrap.ErrListStale) {
		t.Fatalf("rollback: %v", err)
	}
	if err := offer(12, line+"\n", testNow.Add(24*time.Hour), testSigner(t, 8)); !errors.Is(err, bootstrap.ErrListSignature) {
		t.Fatalf("foreign signature: %v", err)
	}
	if err := offer(12, line+"\n", testNow.Add(-time.Hour), s); !errors.Is(err, bootstrap.ErrListExpired) {
		t.Fatalf("expired update: %v", err)
	}
	// the accepted update survives a restart and keeps rejecting the older list
	m2 := bootstrap.OpenManager(bootstrap.ManagerConfig{Builtin: mustBuiltin(t), Keys: []keys.AKey{s.Public()},
		KV: kv, Now: func() time.Time { return testNow }})
	if m2.Seq() != 11 || len(m2.Entries()) != 1 {
		t.Fatalf("stored update lost: seq %d entries %d", m2.Seq(), len(m2.Entries()))
	}
	old, err := bootstrap.SealList(s, 10, uint64(testNow.Add(24*time.Hour).Unix()), line+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.Offer(old); !errors.Is(err, bootstrap.ErrListStale) {
		t.Fatalf("rollback after restart: %v", err)
	}
}

func TestManagerOfferFile(t *testing.T) {
	m, s := newManager(t, newMemKV())
	path := filepath.Join(t.TempDir(), "bootstrap-update.bin")
	raw, err := bootstrap.SealList(s, 12, uint64(testNow.Add(24*time.Hour).Unix()), listA+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := m.OfferFile(path)
	if err != nil || !ok || m.Seq() != 12 {
		t.Fatalf("offer file: ok %v err %v seq %d", ok, err, m.Seq())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the accepted update file was not removed: %v", err)
	}
	// A file that is not signed stays where it is: the refusal is not silent.
	bad := filepath.Join(t.TempDir(), "bootstrap-update.bin")
	if err := os.WriteFile(bad, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.OfferFile(bad); err == nil {
		t.Fatal("an unsigned update was accepted")
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatalf("the refused update was removed: %v", err)
	}
}

func TestServiceCheckAndNoAddressInLog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := &fakeHost{err: errors.New("dial 192.0.2.10:4430: refused")}
	es, err := bootstrap.Parse(listB, testNow)
	if err != nil {
		t.Fatal(err)
	}
	es[0].Builtin, es[0].Index = true, 2
	svc := bootstrap.New(h, es, func() time.Time { return testNow })
	svc.Log(log)
	rep := svc.Run(t.Context())
	if rep.Failed != 1 {
		t.Fatalf("report %+v", rep)
	}
	if strings.Contains(buf.String(), "192.0.2.10") || strings.Contains(buf.String(), hexM) {
		t.Fatalf("the log carries an address: %s", buf.String())
	}
}
