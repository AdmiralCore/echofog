package clock

import (
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// netAddr returns an address in the /16 network number n.
func netAddr(n int) netip.Addr { return netip.AddrFrom4([4]byte{10, byte(n), 0, 1}) }

func newT(t *testing.T) (*Clock, *fakeClock) {
	t.Helper()
	fc := newFakeClock(base)
	return newClock(t, fc.Now, endpoints(3)), fc
}

// TestOffsetNeedsThreeNetworks: handshake.md section 7 - the median is used only when at least 3
// distinct /16 networks gave samples (T3.8-fix1, audit M1 B-2).
func TestOffsetNeedsThreeNetworks(t *testing.T) {
	c, _ := newT(t)
	c.AddOutgoingSample(netAddr(1), base.Add(90*time.Second))
	c.AddOutgoingSample(netAddr(2), base.Add(90*time.Second))
	if got := c.Offset(); got != 0 {
		t.Fatalf("two networks moved the offset to %v", got)
	}
	c.AddOutgoingSample(netAddr(3), base.Add(90*time.Second))
	if got := c.Offset(); got != 90*time.Second {
		t.Fatalf("three agreeing networks: offset %v, want 90s", got)
	}
}

// TestOneSamplePerNetwork: many peers in one /16 count once; three liars in one network are not
// three networks.
func TestOneSamplePerNetwork(t *testing.T) {
	c, _ := newT(t)
	for i := 1; i <= 5; i++ {
		c.AddOutgoingSample(netip.AddrFrom4([4]byte{10, 1, byte(i), 1}), base.Add(100*time.Second))
	}
	if got := c.Offset(); got != 0 {
		t.Fatalf("five peers of one /16 moved the offset to %v", got)
	}
	// IPv6: one /32 is one network.
	for i := 1; i <= 5; i++ {
		c.AddOutgoingSample(netip.MustParseAddr("2001:db8:0:"+string(rune('0'+i))+"::1"), base.Add(100*time.Second))
	}
	if got := c.Offset(); got != 0 {
		t.Fatalf("five peers of one IPv6 /32 moved the offset to %v", got)
	}
}

// TestOffsetStepAndCap: one update moves the offset by at most 120 s and it never exceeds 600 s
// from the raw clock; ten rounds of three liars do not add up (audit M1 B-2).
func TestOffsetStepAndCap(t *testing.T) {
	c, _ := newT(t)
	prev := time.Duration(0)
	for round := 0; round < 10; round++ {
		for n := 1; n <= 3; n++ {
			c.AddOutgoingSample(netAddr(n), base.Add(1000*time.Second)) // liars far beyond the cap
			got := c.Offset()
			if d := got - prev; d > 120*time.Second || d < -120*time.Second {
				t.Fatalf("round %d: one update moved the offset by %v", round, d)
			}
			if got > 600*time.Second || got < -600*time.Second {
				t.Fatalf("round %d: offset %v beyond 600 s", round, got)
			}
			prev = got
		}
	}
	if got := c.Offset(); got != 600*time.Second {
		t.Fatalf("after ten rounds offset %v, want the 600 s cap", got)
	}
}

// TestCDNMayExceedThePeerCap: a larger correction is taken when the clock check agrees.
func TestCDNMayExceedThePeerCap(t *testing.T) {
	fc := newFakeClock(base)
	c := newClock(t, fc.Now, endpoints(3))
	c.mu.Lock()
	c.setOffsetLocked(900 * time.Second) // what MaybeCheckCDN does with an agreeing majority
	c.mu.Unlock()
	if got := c.Offset(); got != 900*time.Second {
		t.Fatalf("CDN offset %v", got)
	}
	// A peer update never pulls it by more than a step, and stays inside the cap logic.
	for n := 1; n <= 3; n++ {
		c.AddOutgoingSample(netAddr(n), base)
	}
	if got := c.Offset(); got < 780*time.Second {
		t.Fatalf("one round of peers moved a CDN offset by %v", 900*time.Second-got)
	}
}

// TestPeersDoNotUndoACDNCorrection: an offset beyond 600 s belongs to the CDN check; honest peers,
// who report the real time, must not wear it down one step at a time (T3.8-fix1 acceptance, finding 1).
func TestPeersDoNotUndoACDNCorrection(t *testing.T) {
	c, _ := newT(t)
	c.mu.Lock()
	c.setOffsetLocked(900 * time.Second)
	c.mu.Unlock()
	for round := 0; round < 4; round++ {
		for n := 1; n <= 3; n++ {
			c.AddOutgoingSample(netAddr(n), base.Add(900*time.Second)) // peers agree with the CDN
		}
	}
	if got := c.Offset(); got != 900*time.Second {
		t.Fatalf("offset %v after agreeing peers, want the CDN's 900 s", got)
	}
	// Disagreeing peers (real time = raw clock + 0) do not erase it either.
	for round := 0; round < 4; round++ {
		for n := 1; n <= 3; n++ {
			c.AddOutgoingSample(netAddr(n), base)
		}
	}
	if got := c.Offset(); got != 900*time.Second {
		t.Fatalf("offset %v after peers that disagree, want the CDN's 900 s kept", got)
	}
}

// TestUnusableAddressIsNotAPeer: a session whose address is unknown gives no sample, and does not
// count as a recent peer that suppresses the CDN check (acceptance finding 3).
func TestUnusableAddressIsNotAPeer(t *testing.T) {
	fc := newFakeClock(base)
	client := &fakeHTTP{fn: func(*http.Request) (*http.Response, error) { return dateAt(fc.Now()), nil }}
	c, err := New(Config{Now: fc.Now, HTTP: client, Endpoints: endpoints(3)})
	if err != nil {
		t.Fatal(err)
	}
	c.AddOutgoingSample(netip.Addr{}, base.Add(5*time.Second))
	res, err := c.MaybeCheckCDN(t.Context(), false)
	if err != nil || !res.Ran {
		t.Fatalf("CDN check suppressed by a sample without an address: res=%+v err=%v", res, err)
	}
}
