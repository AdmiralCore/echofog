package releasekeys_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/releasekeys"
)

func testKey(t testing.TB, n byte) keys.AKey {
	t.Helper()
	seed := sha256.Sum256([]byte{'r', 'k', n})
	s, err := keys.NewEd25519Signer(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	return s.Public()
}

func std(k keys.AKey) string    { return base64.StdEncoding.EncodeToString(k.Key()) }
func rawURL(k keys.AKey) string { return base64.RawURLEncoding.EncodeToString(k.Key()) }
func hexKey(k keys.AKey) string { return hex.EncodeToString(k.Key()) }

func TestParseSpec(t *testing.T) {
	k1, k2, k3 := testKey(t, 1), testKey(t, 2), testKey(t, 3)

	cases := []struct {
		name    string
		spec    string
		want    []keys.AKey
		wantErr bool
	}{
		{name: "empty", spec: ""},
		{name: "whitespace only", spec: " \t\n "},
		{name: "one standard base64", spec: std(k1), want: []keys.AKey{k1}},
		{name: "one raw url base64", spec: rawURL(k1), want: []keys.AKey{k1}},
		{name: "one hex", spec: hexKey(k1), want: []keys.AKey{k1}},
		{name: "akey text form", spec: k1.Text(), want: []keys.AKey{k1}},
		{name: "three keys", spec: std(k1) + "," + std(k2) + "," + std(k3), want: []keys.AKey{k1, k2, k3}},
		{name: "spaces around entries", spec: "  " + std(k1) + " ,\t" + std(k2) + "\n", want: []keys.AKey{k1, k2}},
		{name: "mixed encodings", spec: hexKey(k1) + "," + std(k2) + "," + rawURL(k3), want: []keys.AKey{k1, k2, k3}},
		{name: "bad base64", spec: "!!!not-a-key!!!", wantErr: true},
		{name: "short key", spec: base64.StdEncoding.EncodeToString(make([]byte, 31)), wantErr: true},
		{name: "empty entry", spec: std(k1) + ",,", wantErr: true},
		{name: "bad hex length", spec: strings.Repeat("ab", 31), wantErr: true},
		{name: "small order key", spec: base64.StdEncoding.EncodeToString(identityPoint()), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := releasekeys.ParseSpec(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSpec(%q) = %d keys, want an error", tc.spec, len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSpec(%q): %v", tc.spec, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseSpec(%q) = %d keys, want %d", tc.spec, len(got), len(tc.want))
			}
			for i := range tc.want {
				if !got[i].Equal(tc.want[i]) {
					t.Fatalf("key %d: %s, want %s", i+1, got[i].Text(), tc.want[i].Text())
				}
			}
		})
	}
}

// identityPoint is the canonical encoding of the Ed25519 identity: a small-order key that a
// release key must never be.
func identityPoint() []byte {
	b := make([]byte, 32)
	b[0] = 1
	return b
}

func TestMustParseSpecPanicsOnInvalidSpec(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("MustParseSpec accepted an invalid spec")
		}
		if !strings.Contains(r.(string), "servenode") {
			t.Fatalf("panic message %q does not name the package", r)
		}
	}()
	releasekeys.MustParseSpec("servenode", "%%%")
}

func TestMustParseSpecEmpty(t *testing.T) {
	if got := releasekeys.MustParseSpec("servenode", "  "); len(got) != 0 {
		t.Fatalf("empty spec gave %d keys", len(got))
	}
}
