package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

// testKey is a deterministic 32-byte storage key; a different seed is a different profile. The
// test's cleanup destroys it: locked pages are a finite budget, and memguard panics when it runs
// out (the T4.1 risk).
func testKey(t *testing.T, seed byte) *secret.Value {
	t.Helper()

	key := make([]byte, 32)
	for i := range key {
		key[i] = seed + byte(i)
	}
	value := secret.New(key)
	t.Cleanup(value.Destroy)
	return value
}

func openStore(t *testing.T, dir string, key *secret.Value, memCapMB int) *Store {
	t.Helper()

	st, err := Open(Options{Dir: dir, Key: key, MemCapMB: memCapMB})
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

// readRaw opens the badger directory behind the store's back and returns the raw value.
func readRaw(t *testing.T, dir string, key []byte) []byte {
	t.Helper()

	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open raw badger: %v", err)
	}
	defer db.Close()

	var out []byte
	err = db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		out, err = item.ValueCopy(nil)
		return err
	})
	if err != nil {
		t.Fatalf("read raw key: %v", err)
	}
	return out
}

func rawKey(t *testing.T, ns, key string) []byte {
	t.Helper()

	prefix, err := namespacePrefix(ns)
	if err != nil {
		t.Fatalf("namespacePrefix(%q): %v", ns, err)
	}
	return append(prefix, key...)
}

func TestPathIsTheV2Directory(t *testing.T) {
	t.Parallel()

	data := filepath.Join("profiles", "main")
	if got, want := Path(data), filepath.Join(data, "v2"); got != want {
		t.Fatalf("Path(%q) = %q, want %q", data, got, want)
	}
}

func TestOpenRequiresAStorageKey(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		key  *secret.Value
	}{
		{"nil", nil},
		{"short", secret.New([]byte{1, 2, 3})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Open(Options{Dir: t.TempDir(), Key: tc.key}); !errors.Is(err, ErrBadKey) {
				t.Fatalf("Open with a %s key: %v, want ErrBadKey", tc.name, err)
			}
		})
	}
}

func TestOpenCreatesSchema200(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "v2")
	st := openStore(t, dir, testKey(t, 1), 0)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	version, err := readSchema(dir)
	if err != nil {
		t.Fatalf("readSchema: %v", err)
	}
	if version != Schema {
		t.Fatalf("schema = %d, want %d", version, Schema)
	}
	if Schema != 200 {
		t.Fatalf("Schema = %d, versioning.md §3.7 starts v2 at 200", Schema)
	}

	raw, err := os.ReadFile(filepath.Join(dir, schemaFile))
	if err != nil {
		t.Fatalf("read %s: %v", schemaFile, err)
	}
	if len(raw) < 4 {
		t.Fatalf("%s is %d bytes, want the number first", schemaFile, len(raw))
	}
	if got := binary.BigEndian.Uint32(raw[:4]); got != Schema {
		t.Fatalf("first four bytes of %s = %d, want %d: every store starts with its number", schemaFile, got, Schema)
	}
}

func TestOpenRefusesANewerSchema(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := writeSchema(dir, Schema+1); err != nil {
		t.Fatalf("writeSchema: %v", err)
	}

	_, err := Open(Options{Dir: dir, Key: testKey(t, 1)})
	if !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("Open on schema %d: %v, want ErrSchemaNewer", Schema+1, err)
	}

	version, err := readSchema(dir)
	if err != nil || version != Schema+1 {
		t.Fatalf("the refused store was changed: schema %d, %v", version, err)
	}
	if backups, _ := filepath.Glob(dir + ".backup-*"); len(backups) != 0 {
		t.Fatalf("a refused store was copied: %v", backups)
	}
}

func TestOpenRefusesAnOlderSchemaWithNoMigration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := writeSchema(dir, 150); err != nil {
		t.Fatalf("writeSchema: %v", err)
	}

	_, err := Open(Options{Dir: dir, Key: testKey(t, 1)})
	if !errors.Is(err, ErrSchemaUnknown) {
		t.Fatalf("Open on schema 150 with no migration: %v, want ErrSchemaUnknown", err)
	}
	if backups, _ := filepath.Glob(dir + ".backup-*"); len(backups) != 0 {
		t.Fatalf("an unmigratable store was copied: %v", backups)
	}
}

func TestOpenRefusesADirectoryWithDataButNoSchema(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MANIFEST"), nil, 0o600); err != nil {
		t.Fatalf("write MANIFEST: %v", err)
	}

	if _, err := Open(Options{Dir: dir, Key: testKey(t, 1)}); !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("Open on a database without a schema number: %v, want ErrSchemaMissing", err)
	}
}

func TestNamespaceIsolation(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 2), 0)
	alpha := st.Store("alpha")
	beta := st.Store("beta")

	if err := alpha.Put([]byte("key"), []byte("alpha value")); err != nil {
		t.Fatalf("alpha.Put: %v", err)
	}
	if err := beta.Put([]byte("key"), []byte("beta value")); err != nil {
		t.Fatalf("beta.Put: %v", err)
	}

	for _, tc := range []struct {
		kv   KV
		want string
	}{{alpha, "alpha value"}, {beta, "beta value"}} {
		got, err := tc.kv.Get([]byte("key"))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if string(got) != tc.want {
			t.Fatalf("Get = %q, want %q", got, tc.want)
		}
	}

	entries, err := alpha.Scan(nil, 10)
	if err != nil {
		t.Fatalf("alpha.Scan: %v", err)
	}
	if len(entries) != 1 || string(entries[0].Value) != "alpha value" {
		t.Fatalf("alpha.Scan = %v, want only alpha's record", entries)
	}
}

// TestNamespaceNamesCannotOverlap pins the key encoding: "a" with key "b:c" and "a:b" with key
// "c" must not share a record.
func TestNamespaceNamesCannotOverlap(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 3), 0)
	first := st.Store("a")
	second := st.Store("a:b")

	if err := first.Put([]byte("b:c"), []byte("first")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := second.Put([]byte("c"), []byte("second")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got, err := first.Get([]byte("b:c")); err != nil || string(got) != "first" {
		t.Fatalf("first.Get = %q, %v", got, err)
	}
	if got, err := second.Get([]byte("c")); err != nil || string(got) != "second" {
		t.Fatalf("second.Get = %q, %v", got, err)
	}
	if entries, err := second.Scan(nil, 10); err != nil || len(entries) != 1 {
		t.Fatalf("second.Scan = %v, %v, want one record", entries, err)
	}
}

func TestBadNamespace(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 4), 0)
	for _, ns := range []string{"", strings.Repeat("n", 256)} {
		kv := st.Store(ns)
		if _, err := kv.Get([]byte("k")); !errors.Is(err, ErrBadNamespace) {
			t.Fatalf("Store(%q).Get: %v, want ErrBadNamespace", ns, err)
		}
		if err := kv.Put([]byte("k"), []byte("v")); !errors.Is(err, ErrBadNamespace) {
			t.Fatalf("Store(%q).Put: %v, want ErrBadNamespace", ns, err)
		}
		if _, err := kv.Scan(nil, 1); !errors.Is(err, ErrBadNamespace) {
			t.Fatalf("Store(%q).Scan: %v, want ErrBadNamespace", ns, err)
		}
	}
}

func TestValuesAreEncryptedAtRest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	plain := []byte("the plaintext marker 0123456789")
	st := openStore(t, dir, testKey(t, 5), 0)
	if err := st.Store("chat").Put([]byte("k"), plain); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw := readRaw(t, dir, rawKey(t, "chat", "k"))
	if bytes.Contains(raw, []byte("plaintext marker")) {
		t.Fatal("the stored value contains the plaintext")
	}
	if bytes.Equal(raw, plain) {
		t.Fatal("the stored value is the plaintext")
	}
	// XChaCha20-Poly1305: a 24-byte nonce and a 16-byte tag around the value.
	if want := 24 + len(plain) + 16; len(raw) != want {
		t.Fatalf("stored value is %d bytes, want %d (nonce, ciphertext, tag)", len(raw), want)
	}

	st = openStore(t, dir, testKey(t, 5), 0)
	got, err := st.Store("chat").Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("Get = %q, want %q", got, plain)
	}
}

// TestValuesAreBoundToTheirKey pins the associated data: a value moved under another key of the
// same namespace does not open.
func TestValuesAreBoundToTheirKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := openStore(t, dir, testKey(t, 6), 0)
	if err := st.Store("chat").Put([]byte("a"), []byte("for a")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw := readRaw(t, dir, rawKey(t, "chat", "a"))
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open raw badger: %v", err)
	}
	if err := db.Update(func(txn *badger.Txn) error {
		return txn.Set(rawKey(t, "chat", "b"), raw)
	}); err != nil {
		t.Fatalf("moving the value: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw badger: %v", err)
	}

	st = openStore(t, dir, testKey(t, 6), 0)
	if _, err := st.Store("chat").Get([]byte("b")); !errors.Is(err, ErrValueDamaged) {
		t.Fatalf("Get of a value moved under another key: %v, want ErrValueDamaged", err)
	}
}

// TestValueCannotMoveToAnotherNamespace pins the other half of the associated data: the namespace
// prefix is part of it, so alpha/k moved to beta/k does not open.
func TestValueCannotMoveToAnotherNamespace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := openStore(t, dir, testKey(t, 18), 0)
	if err := st.Store("alpha").Put([]byte("k"), []byte("for alpha")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw := readRaw(t, dir, rawKey(t, "alpha", "k"))
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open raw badger: %v", err)
	}
	if err := db.Update(func(txn *badger.Txn) error {
		return txn.Set(rawKey(t, "beta", "k"), raw)
	}); err != nil {
		t.Fatalf("moving the value: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw badger: %v", err)
	}

	st = openStore(t, dir, testKey(t, 18), 0)
	if _, err := st.Store("beta").Get([]byte("k")); !errors.Is(err, ErrValueDamaged) {
		t.Fatalf("Get of a value moved into another namespace: %v, want ErrValueDamaged", err)
	}
	if got, err := st.Store("alpha").Get([]byte("k")); err != nil || string(got) != "for alpha" {
		t.Fatalf("the original record = %q, %v", got, err)
	}
}

func TestWrongStorageKeyDoesNotOpenValues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := openStore(t, dir, testKey(t, 7), 0)
	if err := st.Store("chat").Put([]byte("k"), []byte("secret")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st = openStore(t, dir, testKey(t, 8), 0)
	if _, err := st.Store("chat").Get([]byte("k")); !errors.Is(err, ErrValueDamaged) {
		t.Fatalf("Get under another storage key: %v, want ErrValueDamaged", err)
	}
}

func TestPutDeleteHasAndEmptyValues(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 9), 0)
	kv := st.Store("test")

	if has, err := kv.Has([]byte("missing")); err != nil || has {
		t.Fatalf("Has(missing) = %v, %v", has, err)
	}
	if _, err := kv.Get([]byte("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
	if err := kv.Put([]byte("empty"), nil); err != nil {
		t.Fatalf("Put(empty): %v", err)
	}
	got, err := kv.Get([]byte("empty"))
	if err != nil || len(got) != 0 {
		t.Fatalf("Get(empty) = %q, %v", got, err)
	}
	if has, err := kv.Has([]byte("empty")); err != nil || !has {
		t.Fatalf("Has(empty) = %v, %v", has, err)
	}
	if err := kv.Delete([]byte("empty")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if has, err := kv.Has([]byte("empty")); err != nil || has {
		t.Fatalf("Has after Delete = %v, %v", has, err)
	}
	if err := kv.Delete([]byte("empty")); err != nil {
		t.Fatalf("Delete of a missing key: %v", err)
	}
}

func TestPutTTLExpires(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 10), 0)
	kv := st.Store("test")

	if err := kv.PutTTL([]byte("k"), []byte("v"), 0); !errors.Is(err, ErrBadTTL) {
		t.Fatalf("PutTTL with no TTL: %v, want ErrBadTTL", err)
	}
	if err := kv.PutTTL([]byte("k"), []byte("v"), time.Second); err != nil {
		t.Fatalf("PutTTL: %v", err)
	}
	if got, err := kv.Get([]byte("k")); err != nil || string(got) != "v" {
		t.Fatalf("Get before expiry = %q, %v", got, err)
	}

	// badger keeps expiry in whole seconds, so the read after the deadline is the assertion.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := kv.Get([]byte("k")); errors.Is(err, ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the value did not expire")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestScanAndScanFrom(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 11), 0)
	kv := st.Store("test")
	for _, key := range []string{"b", "a", "c", "d"} {
		if err := kv.Put([]byte(key), []byte("value "+key)); err != nil {
			t.Fatalf("Put(%s): %v", key, err)
		}
	}

	entries, err := kv.Scan(nil, 10)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := keysOf(entries); got != "a,b,c,d" {
		t.Fatalf("Scan = %s, want a,b,c,d", got)
	}

	entries, err = kv.Scan(nil, 2)
	if err != nil || keysOf(entries) != "a,b" {
		t.Fatalf("Scan with limit 2 = %s, %v", keysOf(entries), err)
	}

	entries, err = kv.Scan([]byte("b"), 10)
	if err != nil || keysOf(entries) != "b" {
		t.Fatalf("Scan(prefix b) = %s, %v", keysOf(entries), err)
	}

	entries, err = kv.ScanFrom(nil, []byte("c"), 2)
	if err != nil || keysOf(entries) != "d" {
		t.Fatalf("ScanFrom(after c) = %s, %v, want d", keysOf(entries), err)
	}

	// A cursor below the prefix starts at the first record of the prefix, not before it.
	entries, err = kv.ScanFrom([]byte("c"), []byte("a"), 10)
	if err != nil || keysOf(entries) != "c" {
		t.Fatalf("ScanFrom(prefix c, after a) = %s, %v, want c", keysOf(entries), err)
	}
	entries, err = kv.ScanFrom([]byte("c"), []byte("c"), 10)
	if err != nil || len(entries) != 0 {
		t.Fatalf("ScanFrom(prefix c, after c) = %s, %v, want empty", keysOf(entries), err)
	}

	for _, limit := range []int{0, -1, MaxScanLimit + 1} {
		if _, err := kv.Scan(nil, limit); !errors.Is(err, ErrBadLimit) {
			t.Fatalf("Scan with limit %d: %v, want ErrBadLimit", limit, err)
		}
	}
}

// TestScanFromPagesThroughEverything checks the contract a paginator uses: hand the last key of
// one page to ScanFrom and get the next records, each exactly once.
func TestScanFromPagesThroughEverything(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 16), 0)
	kv := st.Store("test")
	keys := []string{"aa", "ab", "ac", "ad", "ae", "af", "ag"}
	for _, key := range keys {
		if err := kv.Put([]byte(key), []byte("value "+key)); err != nil {
			t.Fatalf("Put(%s): %v", key, err)
		}
	}

	for _, limit := range []int{1, 2, 3} {
		var got []string
		var cursor []byte
		for pages := 0; ; pages++ {
			if pages > len(keys) {
				t.Fatalf("limit %d: the scan does not end, got %v", limit, got)
			}
			page, err := kv.ScanFrom(nil, cursor, limit)
			if err != nil {
				t.Fatalf("limit %d, page %d: %v", limit, pages, err)
			}
			if len(page) == 0 {
				break
			}
			if len(page) > limit {
				t.Fatalf("limit %d: a page of %d records", limit, len(page))
			}
			for _, entry := range page {
				if cursor != nil && string(entry.Key) <= string(cursor) {
					t.Fatalf("limit %d: page repeats %q after cursor %q", limit, entry.Key, cursor)
				}
				got = append(got, string(entry.Key))
			}
			cursor = page[len(page)-1].Key
		}
		if strings.Join(got, ",") != strings.Join(keys, ",") {
			t.Fatalf("limit %d: paged through %v, want %v", limit, got, keys)
		}
	}

	// The boundaries of the walk.
	for _, tc := range []struct {
		name   string
		cursor string
		want   string
	}{
		{"after the last key", "ag", ""},
		{"beyond the namespace", "zz", ""},
		{"before the first key", "a", strings.Join(keys, ",")},
	} {
		entries, err := kv.ScanFrom(nil, []byte(tc.cursor), 10)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if keysOf(entries) != tc.want {
			t.Fatalf("%s: got %s, want %q", tc.name, keysOf(entries), tc.want)
		}
	}

	// An empty namespace has no pages.
	if entries, err := st.Store("empty").ScanFrom(nil, []byte("a"), 10); err != nil || len(entries) != 0 {
		t.Fatalf("empty namespace: %v, %v", entries, err)
	}
}

// TestScanFromCursorAtAKeyPrefix is the edge the cursor encoding exists for: a key that is a
// proper prefix of another must not drag the longer key into the next page.
func TestScanFromCursorAtAKeyPrefix(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 17), 0)
	kv := st.Store("test")
	for _, key := range []string{"a", "ab", "b"} {
		if err := kv.Put([]byte(key), []byte("value "+key)); err != nil {
			t.Fatalf("Put(%s): %v", key, err)
		}
	}

	if entries, err := kv.ScanFrom(nil, []byte("a"), 10); err != nil || keysOf(entries) != "ab,b" {
		t.Fatalf("after a: %s, %v, want ab,b", keysOf(entries), err)
	}
	if entries, err := kv.ScanFrom(nil, []byte("ab"), 10); err != nil || keysOf(entries) != "b" {
		t.Fatalf("after ab: %s, %v, want b", keysOf(entries), err)
	}
}

func TestScanDecryptsAndKeepsValuesOutOfKeys(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 12), 0)
	kv := st.Store("test")
	if err := kv.Put([]byte("a"), []byte("plain a")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	entries, err := kv.Scan(nil, 10)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 1 || string(entries[0].Key) != "a" || string(entries[0].Value) != "plain a" {
		t.Fatalf("Scan = %v", entries)
	}
}

func TestCloseIsIdempotentAndRefusesLaterUse(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 13), 0)
	kv := st.Store("test")
	if err := kv.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := kv.Get([]byte("k")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: %v, want ErrClosed", err)
	}
	if err := kv.Put([]byte("k"), []byte("v")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close: %v, want ErrClosed", err)
	}
}

func keysOf(entries []Entry) string {
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = string(e.Key)
	}
	return strings.Join(keys, ",")
}

func TestScanResultIsACopy(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 14), 0)
	kv := st.Store("test")
	if err := kv.Put([]byte("a"), []byte("value")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	entries, err := kv.Scan(nil, 10)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	entries[0].Key[0] = 'z'
	entries[0].Value[0] = 'z'
	got, err := kv.Get([]byte("a"))
	if err != nil || string(got) != "value" {
		t.Fatalf("the store changed through the scan result: %q, %v", got, err)
	}
}

func TestConcurrentNamespaces(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(t, 15), 40)
	errs := make(chan error, 8)
	for i := 0; i < cap(errs); i++ {
		go func(i int) {
			kv := st.Store(fmt.Sprintf("ns%d", i%4))
			key := []byte(fmt.Sprintf("%02d", i))
			if err := kv.Put(key, []byte("value")); err != nil {
				errs <- err
				return
			}
			got, err := kv.Get(key)
			if err != nil {
				errs <- err
				return
			}
			if string(got) != "value" {
				errs <- fmt.Errorf("Get = %q", got)
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
