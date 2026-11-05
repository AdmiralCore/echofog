package relay

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// fakeClock is a manually advanced carrier.Clock (AD13).
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
	fired   bool
	c       *fakeClock
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f, c: c}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}

// Advance moves the clock and runs the timers that came due, in order, outside the lock.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	rest := c.timers[:0]
	for _, t := range c.timers {
		switch {
		case t.stopped || t.fired:
		case !t.at.After(c.now):
			t.fired = true
			due = append(due, t)
		default:
			rest = append(rest, t)
		}
	}
	c.timers = rest
	c.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, t := range due {
		t.f()
	}
}

// pending is the number of armed timers.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

// advanceUntil steps the clock by step until cond holds (real-time pauses let goroutines re-arm
// their timers), or fails after max steps.
func advanceUntil(t *testing.T, c *fakeClock, step time.Duration, max int, cond func() bool) {
	t.Helper()
	for i := 0; i < max; i++ {
		if cond() {
			return
		}
		c.Advance(step)
		time.Sleep(15 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not reached by advancing the fake clock")
	}
}
