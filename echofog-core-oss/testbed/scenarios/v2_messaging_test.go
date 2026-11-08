package scenarios

import (
	"testing"
	"time"
)

// v2-messaging: two real nodes meet over noise3 and chat/2 delivers messages through the real
// outbox, with a Directory shared by the two chat services in place of the DHT (the DHT of a
// loopback world does not resolve; the directory is the test's, the chat service, its crypto, the
// outbox and the stream to the peer are the product's).
func TestMessagingScenarioIsGreenInTheModel(t *testing.T) {
	r := runModel(t, "v2-messaging", 5*time.Minute)
	requireGreen(t, r,
		"v2-messaging/session",       // the dial opens a real session
		"v2-messaging/exactly-once",  // both ways, 10 % loss and a delay: every message arrives once, none twice
		"v2-messaging/offline-queue", // the recipient cannot be reached for a while: the outbox keeps the messages, they arrive once it can
		"v2-messaging/state",         // the sender sees every message as delivered
	)
}
