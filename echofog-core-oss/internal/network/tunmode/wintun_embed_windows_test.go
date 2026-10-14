//go:build windows

package tunmode

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedWintunIsAValidDLL(t *testing.T) {
	if len(wintunDLL) == 0 {
		t.Skip("no driver embedded for this architecture")
	}
	if !bytes.HasPrefix(wintunDLL, []byte("MZ")) {
		t.Fatal("embedded wintun does not start with the PE 'MZ' header")
	}
	if len(wintunDLL) < 100_000 {
		t.Fatalf("embedded wintun looks too small: %d bytes", len(wintunDLL))
	}
}

func TestWriteFileIfAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wintun.dll")
	data := []byte("driver-bytes")
	if err := writeFileIfAbsent(path, data); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("file not written correctly: %v / %q", err, got)
	}
	// a second call must not overwrite an existing file
	if err := writeFileIfAbsent(path, []byte("different")); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(path)
	if !bytes.Equal(got2, data) {
		t.Fatalf("existing file was overwritten: %q", got2)
	}
}
