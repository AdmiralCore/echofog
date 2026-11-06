// Package behaviour is the launchpad's behaviour, network and resilience module (T15.0c): the
// censor rules D16–D25 as a deterministic model on a virtual clock, the product's own code driven
// against them (routepolicy, carrier.Plan and carrier.Dial, session.PathPolicy), a netem for the
// real noise3 carrier, and a soak that counts goroutines, descriptors and heap.
//
// Nothing here needs Docker. What does need a live stand (NAT, IPv6, DNS, cut routers, 20 to 5
// nodes, a two-hour soak) is listed in Report.NotChecked and never reported as passed.
package behaviour

import (
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// VClock is a manually advanced carrier.Clock: time moves only when a test or a driver says so,
// so the happy-eyeballs delays, dial timeouts and backoff windows of the product run in virtual
// time and give the same numbers every run.
type VClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*vtimer
}

type vtimer struct {
	c       *VClock
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

var _ carrier.Clock = (*VClock)(nil)

// NewVClock starts at a fixed instant.
func NewVClock() *VClock { return &VClock{now: time.Unix(1_700_000_000, 0)} }

func (c *VClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *VClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &vtimer{c: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *vtimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

// Advance moves the clock forward, running every due callback in time order. A callback may start
// new timers; those that fall inside the step run too.
func (c *VClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *vtimer
		for _, tm := range c.timers {
			if tm.stopped || tm.fired || tm.at.After(target) {
				continue
			}
			if next == nil || tm.at.Before(next.at) {
				next = tm
			}
		}
		if next == nil {
			c.now = target
			// forget the timers that can no longer fire
			kept := c.timers[:0]
			for _, tm := range c.timers {
				if !tm.stopped && !tm.fired {
					kept = append(kept, tm)
				}
			}
			c.timers = kept
			c.mu.Unlock()
			return
		}
		next.fired = true
		c.now = next.at
		f := next.f
		c.mu.Unlock()
		f()
	}
}
