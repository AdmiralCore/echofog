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

// A link that carried 15 of a 20 Mbit/s preset: after the trouble the rate climbs fast to well
// below where it began, then slowly, instead of ten seconds back into the same overflow.
func TestSendRate_ClimbsSlowlyBackToWhereTroubleBegan(t *testing.T) {
	r := &sendRateState{}
	assert.Equal(t, 20000.0, stepCap(r, 1, 20000, pathClean))
	r.sinceCut = rateCooldown + 1
	assert.Equal(t, 15000.0, stepCap(r, 1, 20000, pathHeavy))
	assert.Equal(t, 20000.0, r.ceiling, "the rate the trouble began at")

	stepCap(r, rateCleanRun+5, 20000, pathClean)
	assert.Less(t, r.kbps, 18500.0, "fast only up to near that rate")
	assert.GreaterOrEqual(t, r.kbps, 20000*rateNearCeil)
	at := r.kbps
	stepCap(r, 10, 20000, pathClean)
	assert.Less(t, r.kbps, at*1.12, "then a percent a tick")

	stepCap(r, rateCeilingTTL, 20000, pathClean)
	assert.Zero(t, r.ceiling, "forgotten after a while clean")
	assert.Equal(t, 20000.0, r.kbps)
}

// An overflowing queue halves the rate at once, even right after another cut, and a trouble that
// goes on does not drag the remembered rate down to where it ended.
func TestSendRate_FloodHalvesAndKeepsTheFirstMark(t *testing.T) {
	r := &sendRateState{}
	stepCap(r, 1, 20000, pathClean)
	r.sinceCut = rateCooldown + 1
	assert.Equal(t, 18000.0, stepCap(r, 1, 20000, pathMild))
	assert.Equal(t, 9000.0, stepCap(r, 1, 20000, pathFlood), "no cooldown for an overflow")
	stepCap(r, 30, 20000, pathHeavy)
	assert.Equal(t, 20000.0, r.ceiling, "the mark of the trouble's first cut")
	assert.Equal(t, 600.0, r.kbps)
	stepCap(r, rateCleanRun+40, 20000, pathClean)
	assert.Greater(t, r.kbps, 5000.0, "from a deep cut it climbs fast")
}

func stepCap(r *sendRateState, n int, capKbps float64, level congestion) float64 {
	for i := 0; i < n; i++ {
		r.kbps = r.next(capKbps, 600, level)
	}
	return r.kbps
}
