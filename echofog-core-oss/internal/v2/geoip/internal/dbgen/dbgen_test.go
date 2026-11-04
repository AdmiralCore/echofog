package dbgen

import (
	"compress/gzip"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/geoip/internal/codec"
)

const sampleCSV = `1.0.0.0,1.0.0.255,AU
1.0.1.0,1.0.3.255,ZZ
2.0.0.0,2.0.1.255,US
2001:4860::,2001:4860:ffff:ffff:ffff:ffff:ffff:ffff,US
2a00:1450::,2a00:1450:ffff:ffff:ffff:ffff:ffff:ffff,DE
`

func TestRunPlainCSV(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.csv")
	out := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(in, []byte(sampleCSV), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Run(in, out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.Ranges != 4 || st.IPv4 != 2 || st.IPv6 != 2 || st.Skipped != 1 || st.Countries != 3 {
		t.Fatalf("stats = %+v", st)
	}
	db := open(t, out)
	cases := map[string]string{
		"1.0.0.1":              "AU",
		"2.0.1.1":              "US",
		"2001:4860:4860::8888": "US",
		"2a00:1450::1":         "DE",
	}
	for addr, want := range cases {
		if got, ok := db.Lookup(netip.MustParseAddr(addr)); !ok || got != want {
			t.Errorf("Lookup(%s) = %q, %v; want %q", addr, got, ok, want)
		}
	}
	if _, ok := db.Lookup(netip.MustParseAddr("1.0.1.1")); ok {
		t.Error("unknown (ZZ) range resolved")
	}
}

func TestRunGzipCSV(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.csv.gz")
	f, err := os.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte(sampleCSV)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.bin")
	if _, err := Run(in, out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	db := open(t, out)
	if got, ok := db.Lookup(netip.MustParseAddr("2a00:1450::1")); !ok || got != "DE" {
		t.Fatalf("Lookup = %q, %v; want DE", got, ok)
	}
}

func TestRunRejectsOverlap(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.csv")
	out := filepath.Join(dir, "out.bin")
	data := "1.0.0.0,1.0.0.255,US\n1.0.0.128,1.0.1.255,DE\n"
	if err := os.WriteFile(in, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(in, out); err == nil {
		t.Fatal("Run accepted overlapping ranges")
	}
}

func TestRunRejectsBadLine(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.csv")
	out := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(in, []byte("not-an-ip,1.0.0.1,US\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(in, out); err == nil {
		t.Fatal("Run accepted a malformed address")
	}
}

func open(t *testing.T, path string) *codec.DB {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := codec.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return db
}
