package callnet_test

import "time"

// gateLimits are the release gate of T9.5 (AD21 and the voice limit). The numbers are not
// guesses to be tuned until the test passes: each one is a limit of the task or of AD21, or the
// measurement noise of the in-memory stand, which is stated next to it.
type gateLimits struct {
	// FPSRatio: v2 frame rate over v1 (the baseline transport of the same run), at least.
	FPSRatio float64
	// LatencyRatio: v2 "screen to screen" latency over v1, at most (AD21: v1 + 10 %).
	LatencyRatio float64
	// LatencySlack: absolute allowance on top of the ratio, for the timer noise of the stand.
	LatencySlack time.Duration
	// DirectFPS: 1440p60 direct delivers at least this share of 60 fps ("without degradation").
	DirectFPS float64
	// MaxLoss: share of frames lost on the stand at the nominal rate.
	MaxLoss float64
	// VoiceP95: one-way voice latency, 95th percentile (task: below 250 ms).
	VoiceP95 time.Duration
	// Rehang: the longest silence a listener hears after a relaying member leaves.
	Rehang time.Duration
	// RelayFanout: a relaying viewer sends at most this many times what a viewer receives.
	RelayFanout float64
}

// RED STUB: impossible limits, so the gate fails until the real ones are set.
var gate = gateLimits{
	FPSRatio:     2,
	LatencyRatio: 0.5,
	DirectFPS:    2,
	MaxLoss:      -1,
	VoiceP95:     time.Millisecond,
	Rehang:       time.Millisecond,
	RelayFanout:  0,
}
