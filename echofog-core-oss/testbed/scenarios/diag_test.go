package scenarios

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/behaviour"
)

// When a delivery fails the row has to say what the two ends knew: sessions with their paths and
// idle time, the lab path's counters, and the last events of both runtimes.
func TestTheWhyOfAFailedDeliveryNamesSessionsPathsAndEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	w, a, b, err := messagingWorld(ctx, t.TempDir(), behaviour.NetemConfig{Seed: 1})
	require.NoError(t, err)
	defer w.Close()
	why := offlineWhy(a, b, nil)
	for _, want := range []string{"sender sessions", "receiver sessions", "netem forwarded", "sender events", "receiver events"} {
		assert.Contains(t, why, want)
	}
	assert.NotEmpty(t, a.EventTail(5), "the runtime reported something while it started and dialed")
}
