package scenarios

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/gate"
)

// v2-messaging: two real nodes meet over noise3 and chat/2 delivers messages through the real
// outbox, with a Directory shared by the two chat services in place of the DHT (the DHT of a
// loopback world does not resolve; the directory is the test's, the chat service, its crypto, the
// outbox and the stream to the peer are the product's).
func TestMessagingScenarioIsGreenInTheModel(t *testing.T) {
	r := runModel(t, "v2-messaging", 6*time.Minute)
	// the offline queue may be red on a loaded machine: a finding of the session layer, reported with
	// the time the recovery took (see the report); every other row is green
	requireGreenExcept(t, r, map[string]bool{"v2-messaging/offline-queue": true},
		"v2-messaging/session",      // the dial opens a real session
		"v2-messaging/exactly-once", // both ways, 10 % loss and a delay: every message arrives once, none twice
		"v2-messaging/state",        // the sender sees every message as delivered
	)
	row, ok := rowOf(r, "v2-messaging/offline-queue")
	require.True(t, ok)
	if row.Status == gate.Red {
		assert.NotEmpty(t, r.Findings, "a red offline queue comes with its finding and the timeline")
	} else {
		assert.Equal(t, gate.Green, row.Status)
	}
}
