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

// A planted wintun.dll beside the exe is replaced, never loaded by the elevated helper.
func TestWriteFileReplacingOverwritesAPlantedDLL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wintun.dll")
	if err := os.WriteFile(path, []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileReplacing(path, []byte("trusted")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "trusted" {
		t.Fatalf("planted file kept: %v / %q", err, got)
	}
}
