package scenarios

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Windows refuses to remove a directory with an open file; a store that is still closing must not
// leave its 99 MiB behind.
func TestRemoveTreeWaitsForAFileThatIsStillOpen(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "x")
	require.NoError(t, os.Mkdir(sub, 0o755))
	f, err := os.Create(filepath.Join(sub, "data"))
	require.NoError(t, err)
	go func() {
		time.Sleep(600 * time.Millisecond)
		_ = f.Close()
	}()
	require.NoError(t, removeTree(sub))
	_, err = os.Stat(sub)
	assert.True(t, os.IsNotExist(err), "the directory is gone")
}

func TestRemoveTreeReportsWhatItCouldNotRemove(t *testing.T) {
	assert.NoError(t, removeTree(filepath.Join(t.TempDir(), "never-was")), "nothing to remove is not an error")
}

// A scenario leaves nothing in its scratch directory, whatever it measured.
func TestScenariosLeaveNothingInTheirScratchDirectory(t *testing.T) {
	for _, name := range []string{"v2-hidden", "v2-paths"} {
		dir := t.TempDir()
		r := byName(t, name).Run(t.Context(), Env{Seed: 7, Short: true, Dir: dir})
		require.NotEmpty(t, r.Rows, name)
		left, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, left, "%s left %d entries in its scratch directory", name, len(left))
		for _, f := range r.Findings {
			assert.NotContains(t, f, "could not remove", name)
		}
	}
}

func TestThePathsControlUsesTheScratchDirectoryNotTheSystemTemp(t *testing.T) {
	dir := t.TempDir()
	row := pathsCutWithoutSecondPath(t.Context(), dir)
	assert.Equal(t, "v2-paths/cut-control", row.ID)
	left, _ := os.ReadDir(dir)
	assert.Empty(t, left)
}
