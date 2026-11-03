package handshake

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// vectorDir is docs/protocol/v2/vectors relative to this package.
var vectorDir = filepath.Join("..", "..", "..", "..", "docs", "protocol", "v2", "vectors")

// fixedTime is the clock of every vector (vectors/README.md): 2026-11-03T00:00:00Z.
const fixedTime = 1793664000

var fixedNow = time.Unix(fixedTime, 0)

func loadVector(t testing.TB, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vectorDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return m
}

func obj(t testing.TB, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := m
	for _, k := range keys {
		v, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("vector key %q missing or not an object", k)
		}
		cur = v
	}
	return cur
}

func str(t testing.TB, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("vector key %q missing or not a string", key)
	}
	return s
}

func num(t testing.TB, m map[string]any, key string) uint64 {
	t.Helper()
	f, ok := m[key].(float64)
	if !ok {
		t.Fatalf("vector key %q missing or not a number", key)
	}
	return uint64(f)
}

func hexField(t testing.TB, m map[string]any, key string) []byte {
	t.Helper()
	return mustHex(t, str(t, m, key))
}

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
