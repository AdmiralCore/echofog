// Package jitter spreads timers that shape traffic. A keep-alive, a STUN refresh or a new
// connection every exact N seconds is a clock a censor can read off the wire; each wait is drawn
// anew within a fraction of its nominal length instead.
package jitter

import (
	"math/rand/v2"
	"time"
)

// Default is how far either way a traffic timer moves: 10 s becomes 7.5 to 12.5 s.
const Default = 0.25

// Around returns d moved by a random amount up to frac of d either way. frac is clamped to
// [0, 0.5]; 0 returns d unchanged.
func Around(d time.Duration, frac float64) time.Duration {
	frac = min(max(frac, 0), 0.5)
	span := time.Duration(frac * float64(d))
	if span <= 0 {
		return d
	}
	return d - span + time.Duration(rand.Int64N(int64(2*span)+1))
}
