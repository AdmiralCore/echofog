package handshake

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func TestDifficultyFollowsTheArrivalRate(t *testing.T) {
	c := newClock()
	a := &AntiDDoS{now: c.now}
	assert.Equal(t, uint32(0), a.GetDifficulty(), "nothing is asked of anybody while it is calm")

	burst := func(n int) {
		for i := 0; i < n; i++ {
			a.Note()
		}
	}
	burst(50) // ~17 per second over the window
	assert.Equal(t, uint32(0), a.GetDifficulty())

	burst(100) // 150 in a 3 s window: 50/s
	assert.Equal(t, uint32(busyBits), a.GetDifficulty())

	burst(600) // 750: 250/s
	assert.Equal(t, uint32(heavyBits), a.GetDifficulty())

	burst(2000) // 2750: 917/s
	assert.Equal(t, uint32(floodBits), a.GetDifficulty())

	// the flood ends: after the window passes the node is calm again
	c.advance(5 * time.Second)
	assert.Equal(t, uint32(0), a.GetDifficulty())
}

func TestRateIgnoresOldTraffic(t *testing.T) {
	c := newClock()
	a := &AntiDDoS{now: c.now}
	for i := 0; i < 300; i++ {
		a.Note()
	}
	assert.InDelta(t, 100, a.Rate(), 0.1)
	c.advance(2 * time.Second)
	assert.InDelta(t, 100, a.Rate(), 0.1, "still inside the window")
	c.advance(2 * time.Second)
	assert.Zero(t, a.Rate())
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func newTestGuard(cfg GuardConfig) (*Guard, *clock) {
	c := newClock()
	cfg.CountLoopback = true
	g := NewGuard(cfg)
	g.now = c.now
	return g, c
}

func TestGuardLimitsConnectionBursts(t *testing.T) {
	g, c := newTestGuard(GuardConfig{Burst: 5, Refill: 1})
	for i := 0; i < 5; i++ {
		release, ok := g.Allow(ip("203.0.113.7"))
		require.True(t, ok, "attempt %d", i)
		release()
	}
	_, ok := g.Allow(ip("203.0.113.7"))
	assert.False(t, ok, "the burst is used up")

	_, ok = g.Allow(ip("203.0.113.8"))
	assert.True(t, ok, "another address is not affected")

	c.advance(2 * time.Second)
	release, ok := g.Allow(ip("203.0.113.7"))
	assert.True(t, ok, "attempts come back over time")
	release()
}

func TestGuardLimitsParallelHandshakesOfOneAddress(t *testing.T) {
	g, _ := newTestGuard(GuardConfig{Burst: 100, MaxActive: 2})
	r1, ok1 := g.Allow(ip("203.0.113.7"))
	r2, ok2 := g.Allow(ip("203.0.113.7"))
	require.True(t, ok1 && ok2)
	_, ok := g.Allow(ip("203.0.113.7"))
	assert.False(t, ok)
	r1()
	r1() // releasing twice must not free a second slot
	_, ok = g.Allow(ip("203.0.113.7"))
	assert.True(t, ok)
	_, ok = g.Allow(ip("203.0.113.7"))
	assert.False(t, ok)
	r2()
}

func TestGuardBansAddressesThatKeepFailing(t *testing.T) {
	g, c := newTestGuard(GuardConfig{Burst: 1000, FailLimit: 3, BanFor: time.Minute})
	a := ip("198.51.100.9")
	for i := 0; i < 3; i++ {
		release, ok := g.Allow(a)
		require.True(t, ok)
		release()
		g.Fail(a)
	}
	assert.True(t, g.Banned(a))
	_, ok := g.Allow(a)
	assert.False(t, ok, "a banned address is turned away")

	c.advance(61 * time.Second)
	release, ok := g.Allow(a)
	assert.True(t, ok, "the ban ends")
	release()
	assert.False(t, g.Banned(a))
}

func TestGuardForgivesAfterSuccessAndOldFailures(t *testing.T) {
	g, c := newTestGuard(GuardConfig{Burst: 1000, FailLimit: 3, FailWindow: time.Minute})
	a := ip("198.51.100.9")
	g.Fail(a)
	g.Fail(a)
	g.Success(a) // a proper peer: the earlier trouble is forgotten
	g.Fail(a)
	g.Fail(a)
	assert.False(t, g.Banned(a))

	c.advance(2 * time.Minute) // failures far apart do not add up
	g.Fail(a)
	assert.False(t, g.Banned(a))
}

func TestGuardDoesNotLimitLocalConnectionsByDefault(t *testing.T) {
	g := NewGuard(GuardConfig{Burst: 1, FailLimit: 1})
	for i := 0; i < 50; i++ {
		release, ok := g.Allow(ip("127.0.0.1"))
		require.True(t, ok)
		release()
		g.Fail(ip("127.0.0.1"))
	}
	assert.False(t, g.Banned(ip("127.0.0.1")))
}

func TestGuardMapsIPv4InIPv6AndStaysBounded(t *testing.T) {
	g, _ := newTestGuard(GuardConfig{Burst: 1})
	_, ok := g.Allow(ip("::ffff:203.0.113.7"))
	require.True(t, ok)
	_, ok = g.Allow(ip("203.0.113.7"))
	assert.False(t, ok, "the same host in two spellings shares one budget")

	for i := 0; i < maxTrackedAddrs+500; i++ {
		a := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		_, _ = g.Allow(a)
	}
	g.mu.Lock()
	n := len(g.ips)
	g.mu.Unlock()
	assert.LessOrEqual(t, n, maxTrackedAddrs+1)
}
