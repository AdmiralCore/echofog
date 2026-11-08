package scenarios

import (
	"testing"
	"time"
)

// v2-messaging: two real nodes, linked by a contact link through a real handshake, send each other
// messages through the real chat outbox over noise3 behind a lossy netem.
func TestMessagingScenarioIsGreenInTheModel(t *testing.T) {
	r := runModel(t, "v2-messaging", 4*time.Minute)
	requireGreen(t, r,
		"v2-messaging/link",          // the contact link opens a real session
		"v2-messaging/exactly-once",  // both ways, 10 % loss and a delay: every message arrives once, none twice
		"v2-messaging/offline-queue", // the recipient cannot be reached for a while: the outbox keeps the messages, they arrive once it can
		"v2-messaging/state",         // the sender sees every message as delivered
	)
}
