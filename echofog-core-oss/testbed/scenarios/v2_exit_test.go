package scenarios

import (
	"testing"
	"time"
)

// v2-exit: the product's own exit client and exit server over real noise3 behind a lossy netem, a
// web server standing in for the Internet.
func TestExitScenarioIsGreenInTheModel(t *testing.T) {
	r := runModel(t, "v2-exit", 3*time.Minute)
	requireGreen(t, r,
		"v2-exit/download",        // a file comes through whole, hash equal, over a lossy path
		"v2-exit/private-refused", // the product's exit refuses a loopback target (SSRF)
		"v2-exit/failover",        // the first exit goes away: the client uses the second
		"v2-exit/resources",       // after many tunnels the exit side has given its goroutines back
	)
}
