package servernode

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/releasekeys"
)

// TestReleaseKeysMatchBuildSpec checks the init wiring of the build-time key list. A release build
// passes "-ldflags -X github.com/echofog/echofog-core/internal/v2/servernode.releaseKeysSpec=...";
// run this test with and without that flag. It also pins the fail-closed OSS build: an empty spec
// must leave both key sets empty.
func TestReleaseKeysMatchBuildSpec(t *testing.T) {
	want, err := releasekeys.ParseSpec(releaseKeysSpec)
	if err != nil {
		t.Fatalf("releaseKeysSpec does not parse: %v", err)
	}
	checkBuildKeys(t, "buildReleaseKeys", buildReleaseKeys, want)
	// No test that runs before this file changes ReleaseKeys; later tests save and restore it.
	checkBuildKeys(t, "ReleaseKeys", ReleaseKeys, want)
}

func checkBuildKeys(t *testing.T, name string, got, want []keys.AKey) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s has %d keys, the build spec has %d", name, len(got), len(want))
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("%s key %d is %s, want %s", name, i+1, got[i].Text(), want[i].Text())
		}
	}
}
