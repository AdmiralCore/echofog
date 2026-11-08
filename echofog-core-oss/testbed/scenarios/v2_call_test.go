package scenarios

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/echofog/echofog-core/testbed/gate"
)

// v2-call: the product's call transport over real noise3. Media frames go from a to b through a
// netem with 5 % loss and 75 ms each way (a 150 ms round trip).
//
// A voice call, the clean path, the direct path and a video call at half the frame rate are green.
// A video call at 30 frames a second is a finding of the product when it queues its media after a
// loss (see the report): its two rows may be red, and then the scenario says why in Findings. Nothing
// else may be red, and the limits are never moved.
func TestCallScenarioIsGreenExceptTheVideoCallFinding(t *testing.T) {
	r := runModel(t, "v2-call", 5*time.Minute)
	requireGreenExcept(t, r, map[string]bool{"v2-call/video-call-voice": true, "v2-call/video-loss": true},
		"v2-call/clean-path", "v2-call/voice-loss", "v2-call/direct-path", "v2-call/video-call-half-rate", "v2-call/video-call-half-rate-video")
	red := false
	for _, id := range []string{"v2-call/video-call-voice", "v2-call/video-loss"} {
		row, ok := rowOf(r, id)
		assert.True(t, ok, id)
		red = red || row.Status == gate.Red
	}
	if red {
		assert.NotEmpty(t, r.Findings, "a red video call comes with a finding")
	}
}
