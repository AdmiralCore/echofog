//go:build !race

package testutil

import "time"

// Slow returns the budget unchanged outside the race detector.
func Slow(d time.Duration) time.Duration { return d }
