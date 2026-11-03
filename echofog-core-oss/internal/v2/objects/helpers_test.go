package objects

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
)

// vectorDir is docs/protocol/v2/vectors relative to this package.
var vectorDir = filepath.Join("..", "..", "..", "..", "docs", "protocol", "v2", "vectors")

// fixedTime is the clock of every vector: 2026-11-03T00:00:00Z.
const fixedTime uint64 = 1793664000

var vectorNow = time.Unix(int64(fixedTime), 0)

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

// get walks a path of object keys.
func get(t testing.TB, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		o, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("vector path %v: %q is not an object", path, p)
		}
		cur, ok = o[p]
		if !ok {
			t.Fatalf("vector path %v: missing %q", path, p)
		}
	}
	return cur
}

func str(t testing.TB, m map[string]any, path ...string) string {
	t.Helper()
	s, ok := get(t, m, path...).(string)
	if !ok {
		t.Fatalf("vector path %v is not a string", path)
	}
	return s
}

func num(t testing.TB, m map[string]any, path ...string) uint64 {
	t.Helper()
	f, ok := get(t, m, path...).(float64)
	if !ok {
		t.Fatalf("vector path %v is not a number", path)
	}
	return uint64(f)
}

func hx(t testing.TB, m map[string]any, path ...string) []byte {
	t.Helper()
	return unhex(t, str(t, m, path...))
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

func sha(b ...[]byte) []byte {
	h := sha256.New()
	for _, p := range b {
		h.Write(p)
	}
	return h.Sum(nil)
}

func signer(t testing.TB, seedHex string) *keys.Signer {
	t.Helper()
	s, err := keys.NewEd25519Signer(unhex(t, seedHex))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// testSeed derives a deterministic seed for tests that need extra keys.
func testSeed(name string) []byte {
	h := sha256.Sum256([]byte("echofog v2 objects test seed: " + name))
	return h[:]
}

func testSigner(t testing.TB, name string) *keys.Signer {
	t.Helper()
	s, err := keys.NewEd25519Signer(testSeed(name))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func eqHex(t testing.TB, what string, got, want []byte) {
	t.Helper()
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("%s mismatch\n got: %x\nwant: %x", what, got, want)
	}
}

// sealRaw builds a signed envelope without any body check, for inputs that must be rejected.
func sealRaw(label string, s *keys.Signer, seq, notAfter uint64, body []byte) []byte {
	out := []byte{FormatV2, byte(len(label))}
	out = append(out, label...)
	out = append(out, s.Public()...)
	out = binary.BigEndian.AppendUint64(out, seq)
	out = binary.BigEndian.AppendUint64(out, notAfter)
	out = appendVarint(out, uint64(len(body)))
	out = append(out, body...)
	return append(out, s.Sign(out)...)
}
