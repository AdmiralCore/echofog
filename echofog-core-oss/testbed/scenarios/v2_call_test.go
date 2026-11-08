package scenarios

import (
	"testing"
	"time"
)

// v2-call: the product's call transport over real noise3. Media frames go from a to b through a
// netem with 5 % loss and 75 ms each way (a 150 ms round trip); voice and video are counted.
func TestCallScenarioIsGreenInTheModel(t *testing.T) {
	r := runModel(t, "v2-call", 3*time.Minute)
	requireGreen(t, r,
		"v2-call/clean-path",  // no loss: every frame arrives, none twice
		"v2-call/voice-loss",  // 5 % loss, 150 ms: at least 90 % of the voice frames, in time
		"v2-call/video-loss",  // the same for video
		"v2-call/direct-path", // call_route=direct: the datagrams ride the direct path
	)
}
