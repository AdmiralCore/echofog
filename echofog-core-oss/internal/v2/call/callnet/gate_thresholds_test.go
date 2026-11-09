package callnet_test

import "time"

// gateLimits are the release gate of T9.5. All comparisons with v1 are relative: both halves of
// a ratio come from back-to-back runs on the same stand, so the load of the machine moves them
// together, and the gate takes the median of several pairs. Absolute numbers (fps, milliseconds)
// in the logs and the report are for reference only. The two absolute limits are the task's own:
// voice below 250 ms one way, 1440p60 direct without degradation.
//
// Nothing here was tuned until a run went green: AD21 and the task fix the ratios, and the slack
// values are the measured timer noise of the stand (see reports/T9.5.md).
type gateLimits struct {
	// FPSRatio: v2 frame rate over v1, at least (AD21: the same fps; the 3 % is the stand's counting
	// granularity over a 5.7 s window).
	FPSRatio float64
	// LatencyRatio: v2 "screen to screen" latency over v1, at most (AD21: v1 + 10 %).
	LatencyRatio float64
	// LatencySlack: absolute allowance on top of the ratio, for the timer noise of the stand.
	LatencySlack time.Duration
	// DirectFPS: 1440p60 direct delivers at least this share of 60 fps ("without degradation").
	DirectFPS float64
	// MaxLoss: frames v2 may lose beyond what v1 loses in the same pairs (median of the repeats).
	MaxLoss float64
	// VoiceP95: one-way voice latency, 95th percentile (task: below 250 ms), with 40 ms added per hop.
	VoiceP95 time.Duration
	// RehangRatio and Rehang: the longest silence a listener hears after a relaying member leaves
	// or crashes, at most RehangRatio × v1's plus Rehang of slack.
	RehangRatio float64
	Rehang      time.Duration
	// RelayFanout: a relaying viewer sends at most this many times what a viewer receives (the
	// declared fan-out of 2, plus 10 %).
	RelayFanout float64
}

var gate = gateLimits{
	FPSRatio:     0.97,
	LatencyRatio: 1.10,
	LatencySlack: 3 * time.Millisecond,
	DirectFPS:    0.95,
	MaxLoss:      0.02,
	VoiceP95:     250 * time.Millisecond,
	RehangRatio:  1.10,
	Rehang:       250 * time.Millisecond,
	RelayFanout:  2.2,
}
