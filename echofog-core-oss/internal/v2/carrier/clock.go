package carrier

import "time"

// Timer is the part of a timer the dial race needs: cancellation of a pending start or timeout.
type Timer interface {
	Stop() bool
}

// Clock is the injectable time source of the carrier layer (AD13). Production uses SystemClock;
// tests use a manually advanced clock so happy-eyeballs delays and dial timeouts are deterministic.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
