package node

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTunnelRateTakesOnlyBusySettledStretches(t *testing.T) {
	since := time.Unix(1_000_000, 0)
	at := since.Add(time.Minute)
	var r tunnelRate

	_, ok := r.sample("x1", 0, since, at)
	assert.False(t, ok, "the first sample has nothing to compare with")

	kbps, ok := r.sample("x1", 3<<20, since, at.Add(3*time.Second))
	assert.True(t, ok)
	assert.InDelta(t, 3*(1<<20)*8/1000/3, kbps, 1, "3 MiB in 3 s")

	_, ok = r.sample("x1", 3<<20+500<<10, since, at.Add(6*time.Second))
	assert.False(t, ok, "half a MiB in a round: the user asked for little")

	_, ok = r.sample("x2", 10<<20, since, at.Add(9*time.Second))
	assert.False(t, ok, "the bytes since the last sample went through the previous exit too")
	_, ok = r.sample("x2", 20<<20, since, at.Add(12*time.Second))
	assert.True(t, ok)

	_, ok = r.sample("x2", 1<<20, since, at.Add(15*time.Second))
	assert.False(t, ok, "the proxy's counter went back: it was restarted")

	var fresh tunnelRate
	switched := at
	fresh.sample("x3", 0, switched, switched.Add(3*time.Second))
	_, ok = fresh.sample("x3", 10<<20, switched, switched.Add(6*time.Second))
	assert.False(t, ok, "right after a switch, connections still open on the previous exit count as well")
	fresh.sample("x3", 20<<20, switched, switched.Add(rateSettle))
	_, ok = fresh.sample("x3", 30<<20, switched, switched.Add(rateSettle+3*time.Second))
	assert.True(t, ok)
}
