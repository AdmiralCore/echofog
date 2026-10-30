package call

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// step runs n ticks at one level and returns the rate.
func step(r *sendRateState, n int, level congestion) float64 {
	for i := 0; i < n; i++ {
		r.kbps = r.next(10000, 600, level)
	}
	return r.kbps
}

func TestSendRate_CutsGentlyThenWaitsAndClimbsBackToThePreset(t *testing.T) {
	r := &sendRateState{}
	assert.Equal(t, 10000.0, step(r, 1, pathClean), "starts at the preset")

	r.sinceCut = rateCooldown + 1
	assert.Equal(t, 9000.0, step(r, 1, pathMild), "a few drops cut a little")
	assert.Equal(t, 9000.0, step(r, rateCooldown, pathMild), "then it waits for the cut to show")
	assert.Equal(t, 6750.0, step(r, 1, pathHeavy), "a full queue cuts more")

	assert.Equal(t, 6750.0, step(r, rateCleanRun-1, pathClean), "no climbing right after trouble")
	assert.Greater(t, step(r, 1, pathClean), 6750.0, "climbs once the path stayed clean")

	assert.Equal(t, 600.0, step(r, 200, pathHeavy), "never below the floor")
	assert.Equal(t, 10000.0, step(r, 400, pathClean), "never above the preset")
}
