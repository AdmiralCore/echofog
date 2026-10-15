package tunmode

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

// setDWordFlags must set the flags to 1 and, on revert, restore exactly the prior state: a value
// that existed is put back to its old number, and a value that did not exist is removed. This is
// what enableDNSLeakProtection relies on so turning the VPN off leaves the DNS policy as it found
// it. The test uses a scratch key under HKCU so it needs no administrator rights.
func TestSetDWordFlags_SnapshotAndRestore(t *testing.T) {
	const path = `Software\EchoFogTest\dnsleak`
	defer registry.DeleteKey(registry.CURRENT_USER, path)

	// Pre-seed one value so we can prove it is restored, and leave the other absent.
	seed, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("create scratch key: %v", err)
	}
	if err := seed.SetDWordValue("Existing", 7); err != nil {
		t.Fatalf("seed value: %v", err)
	}
	seed.Close()

	revert, err := setDWordFlags(registry.CURRENT_USER, path, []string{"Existing", "Fresh"})
	if err != nil {
		t.Fatalf("setDWordFlags: %v", err)
	}

	// While active both are 1.
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, n := range []string{"Existing", "Fresh"} {
		if v, _, err := k.GetIntegerValue(n); err != nil || v != 1 {
			t.Errorf("%s = %d (err %v), want 1", n, v, err)
		}
	}
	k.Close()

	revert()

	k2, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("open after revert: %v", err)
	}
	defer k2.Close()
	if v, _, err := k2.GetIntegerValue("Existing"); err != nil || v != 7 {
		t.Errorf("Existing after revert = %d (err %v), want 7", v, err)
	}
	if _, _, err := k2.GetIntegerValue("Fresh"); err == nil {
		t.Error("Fresh should have been removed on revert, but it is still present")
	}
}
