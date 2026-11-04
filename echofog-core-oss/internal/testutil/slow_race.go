//go:build race

package testutil

import "time"

// Slow stretches a wall-clock budget for a test running under the race detector. The race
// detector runs the same code several times slower than a normal build, and CI gives it two
// cores, so a budget that fits a developer machine is too tight there. It only stretches
// deadlines and sampling windows; it never changes what a test asserts.
func Slow(d time.Duration) time.Duration { return 4 * d }
