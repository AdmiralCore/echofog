package noise

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// vectorDir is docs/protocol/v2/vectors relative to this package.
var vectorDir = filepath.Join("..", "..", "..", "..", "docs", "protocol", "v2", "vectors")

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

func hexField(t testing.TB, m map[string]any, key string) []byte {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("vector key %q missing or not a string", key)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("vector key %q: %v", key, err)
	}
	return b
}

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
