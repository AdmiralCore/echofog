//go:build windows

package tunmode

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
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

// The driver lands in its folder once, an identical copy is left alone and a different one is
// replaced.
func TestInstallWintunWritesOnlyWhatDiffers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "EchoFog")
	path, err := installWintun(dir, []byte("trusted"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("installed at %s, want in %s", path, dir)
	}
	before, _ := os.Stat(path)
	if _, err := installWintun(dir, []byte("trusted")); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an identical driver was written again")
	}
	if err := os.WriteFile(path, []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installWintun(dir, []byte("trusted")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "trusted" {
		t.Fatalf("a different driver was kept: %q", got)
	}
}

// What loadWintun relies on: once the driver is loaded by its full path, the tunnel library's load
// by name (application folder, then System32) gets that module, not a file beside the exe.
func TestWintunLoadedByPathIsTheOneLoadedByName(t *testing.T) {
	if len(wintunDLL) == 0 {
		t.Skip("no driver embedded for this architecture")
	}
	path, err := installWintun(t.TempDir(), wintunDLL)
	if err != nil {
		t.Fatal(err)
	}
	byPath, err := windows.LoadLibraryEx(path, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.FreeLibrary(byPath) // a loaded DLL cannot be deleted with the temp folder
	const searchAppDir = 0x00000200 // LOAD_LIBRARY_SEARCH_APPLICATION_DIR, as golang.zx2c4.com/wintun uses
	byName, err := windows.LoadLibraryEx("wintun.dll", 0, searchAppDir|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		t.Fatalf("the load by name did not find the loaded driver: %v", err)
	}
	defer windows.FreeLibrary(byName)
	if byName != byPath {
		t.Fatal("the load by name got another module than the trusted one")
	}
}

func TestToolsRunFromSystem32(t *testing.T) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if got := system32("netsh.exe"); got != filepath.Join(sys, "netsh.exe") {
		t.Fatalf("netsh from %s", got)
	}
	if _, err := os.Stat(system32("netsh.exe")); err != nil {
		t.Fatal(err)
	}
}
