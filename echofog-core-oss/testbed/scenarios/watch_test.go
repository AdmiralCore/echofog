package scenarios

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// A short run is what go test makes: the whole package has to fit in go test's default ten minutes
// even when the offline queue is red, so the short watch is a few minutes. The run for the gate watches
// the full time.
func TestOfflineWatchIsShortInAShortRun(t *testing.T) {
	assert.LessOrEqual(t, offlineWatchFor(Env{Short: true}), 3*time.Minute)
	assert.Equal(t, offlineWatch, offlineWatchFor(Env{}))
	assert.Greater(t, offlineWatchFor(Env{Short: true}), offlineLimit, "the short watch still outlasts the limit of the row")
}
