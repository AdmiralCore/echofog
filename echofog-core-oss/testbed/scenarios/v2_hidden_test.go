package scenarios

import (
	"testing"
	"time"
)

// v2-hidden: a hidden node with no public card, reached only through an invitation link, over real
// noise3. A stranger gets silence.
func TestHiddenScenarioIsGreenInTheModel(t *testing.T) {
	r := runModel(t, "v2-hidden", 2*time.Minute)
	requireGreen(t, r,
		"v2-hidden/not-listed", // no public card, nothing in the DHT
		"v2-hidden/invited-in", // the invitation link opens a real session
		"v2-hidden/stranger",   // junk and a dial without the secret get no answer
		"v2-hidden/revoked",    // after the pass is revoked the same link does not open
	)
}
