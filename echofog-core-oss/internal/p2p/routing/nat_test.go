package routing

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/natcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func seen(pairs ...string) Observer {
	return func(context.Context) []natcheck.Observation {
		var out []natcheck.Observation
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, natcheck.Observation{From: pairs[i], Addr: netip.MustParseAddrPort(pairs[i+1])})
		}
		return out
	}
}

func discover(t *testing.T, o Observer) *NATManager {
	m := NewNATManager(nil, quiet())
	m.SetObserver(o)
	_, err := m.Discover()
	require.NoError(t, err)
	return m
}

func TestConeNATCanBePunchedButNotBeARouter(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:50000", "b", "203.0.113.7:50000"))
	addr, typ := m.GetNATProfile()
	assert.Equal(t, "203.0.113.7:50000", addr)
	assert.Equal(t, NATFullCone, typ)
	assert.True(t, m.CanBePunched())
	assert.False(t, m.CanBeRouter(), "a cone NAT lets answers in, not strangers")
}

func TestSymmetricNATCannotBePunched(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:50000", "b", "203.0.113.7:50777"))
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATSymmetric, typ)
	assert.False(t, m.CanBePunched())
	assert.False(t, m.CanBeRouter())
}

// A symmetric NAT whose router forwards our port: that port is one public address for everybody,
// so peers reach us there (it was taken for unreachable, and nothing was even tried).
func TestForwardedPortBeatsASymmetricNAT(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:50000", "b", "203.0.113.7:50777"))
	m.SetPortMapping(netip.MustParseAddrPort("203.0.113.7:49829"))
	assert.True(t, m.CanBePunched())
	assert.True(t, m.CanBeRouter())
	assert.Equal(t, "203.0.113.7:49829", m.Candidates()[0])
}

func TestOneObserverIsWorthATry(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:50000"))
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATUnknown, typ)
	assert.True(t, m.CanBePunched(), "unknown is not a reason to give up")
	assert.False(t, m.CanBeRouter())
}

func TestAForwardedPortMakesUsReachableEvenBehindNAT(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:50000", "b", "203.0.113.7:50000"))
	require.False(t, m.CanBeRouter())

	m.SetPortMapping(netip.MustParseAddrPort("203.0.113.7:4242"))
	assert.True(t, m.CanBeRouter())
	assert.Equal(t, []string{"203.0.113.7:4242", "203.0.113.7:50000"}, m.Candidates(), "the forwarded address first")

	m.SetPortMapping(netip.AddrPort{})
	assert.False(t, m.CanBeRouter(), "the router dropped the forward")
}

func TestCandidatesDoNotRepeatTheSameAddress(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:4242", "b", "203.0.113.7:4242"))
	m.SetPortMapping(netip.MustParseAddrPort("203.0.113.7:4242"))
	assert.Equal(t, []string{"203.0.113.7:4242"}, m.Candidates())
}

func TestNoNATWhenTheObservedAddressIsOurOwn(t *testing.T) {
	local := natcheck.LocalAddrs()
	var v4 netip.Addr
	for _, a := range local {
		if a.Is4() {
			v4 = a
			break
		}
	}
	if !v4.IsValid() {
		t.Skip("no IPv4 interface address on this machine")
	}
	ap := netip.AddrPortFrom(v4, 4242).String()
	m := discover(t, seen("a", ap, "b", ap))
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATPublic, typ)
	assert.True(t, m.CanBeRouter())
}

func TestDiscoverFailsClearlyWhenNobodyAnswers(t *testing.T) {
	m := NewNATManager(nil, quiet())
	_, err := m.Discover()
	assert.Error(t, err)
	m.SetObserver(seen())
	_, err = m.Discover()
	assert.Error(t, err)
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATUnknown, typ, "a failed check must not invent a type")
}

func TestTheTypeFollowsTheNetworkWhenItChanges(t *testing.T) {
	obs := seen("a", "203.0.113.7:50000", "b", "203.0.113.7:50000")
	m := NewNATManager(nil, quiet())
	m.SetObserver(func(ctx context.Context) []natcheck.Observation { return obs(ctx) })
	_, _ = m.Discover()
	_, typ := m.GetNATProfile()
	require.Equal(t, NATFullCone, typ)

	obs = seen("a", "203.0.113.7:50000", "b", "198.51.100.1:51000") // moved to a carrier NAT
	_, _ = m.Discover()
	_, typ = m.GetNATProfile()
	assert.Equal(t, NATSymmetric, typ)
}

func TestSTUNServersCanBeOverriddenAndEmptyKeepsDefaults(t *testing.T) {
	m := NewNATManager(nil, quiet())
	m.SetSTUNServers(nil)
	assert.Equal(t, natcheck.DefaultSTUNServers, m.servers)
	m.SetSTUNServers([]string{"stun.example.net:3478"})
	assert.Equal(t, []string{"stun.example.net:3478"}, m.servers)
}

// stunCounter is a QUIC socket's STUN side that counts what is sent on it and never answers.
type stunCounter struct {
	mu       sync.Mutex
	writes   int
	deadline time.Time
	closed   chan struct{}
}

func newSTUNCounter() *stunCounter { return &stunCounter{closed: make(chan struct{})} }

func (c *stunCounter) ReadFrom([]byte) (int, net.Addr, error) {
	c.mu.Lock()
	d := c.deadline
	c.mu.Unlock()
	wait := time.Until(d)
	if d.IsZero() || wait > time.Second {
		wait = time.Second
	}
	select {
	case <-time.After(wait):
	case <-c.closed:
	}
	return 0, nil, os.ErrDeadlineExceeded
}

func (c *stunCounter) WriteTo(b []byte, _ net.Addr) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return len(b), nil
}
func (c *stunCounter) Close() error { close(c.closed); return nil }
func (c *stunCounter) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4242}
}
func (c *stunCounter) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }
func (c *stunCounter) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}
func (c *stunCounter) SetWriteDeadline(time.Time) error { return nil }
func (c *stunCounter) sent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

// Two connected peers with public addresses tell the public address and the NAT kind: no request
// goes to a public STUN server, and no STUN keep-alive either (the peers' QUIC keep-alives hold
// the mapping open).
func TestPeersReplaceSTUNWhenTwoOfThemAnswer(t *testing.T) {
	conn := newSTUNCounter()
	m := NewNATManager(conn, quiet())
	m.SetSTUNServers([]string{"127.0.0.1:3478"})
	m.SetObserver(seen("server1", "203.0.113.7:50000", "server2", "203.0.113.7:50777"))
	_, err := m.Discover()
	require.NoError(t, err)
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATSymmetric, typ, "learned from the peers alone")
	assert.Equal(t, 0, conn.sent(), "no STUN request when peers answered")
	assert.Equal(t, 0, m.STUNChecks())

	lo, hi, n := m.PortRange()
	assert.Equal(t, uint16(50000), lo)
	assert.Equal(t, uint16(50777), hi)
	assert.Equal(t, 2, n)

	m.ping(context.Background())
	assert.Equal(t, 0, conn.sent(), "connected peers keep the mapping open: no STUN keep-alive")
}

// With fewer than two peers (just started, or one peer) STUN is the fallback; peers on our LAN do
// not count, their view is from inside the NAT.
func TestSTUNIsTheFallbackWhenTooFewPeersAnswer(t *testing.T) {
	for name, o := range map[string]Observer{
		"no peer":      seen(),
		"one peer":     seen("server1", "203.0.113.7:50000"),
		"only on LAN":  seen("lan1", "192.168.1.5:4242", "lan2", "192.168.1.5:4242"),
		"one observer": seen("server1", "203.0.113.7:50000", "server1", "203.0.113.7:50000"),
	} {
		conn := newSTUNCounter()
		m := NewNATManager(conn, quiet())
		m.SetSTUNServers([]string{"127.0.0.1:3478"})
		m.SetObserver(o)
		_, _ = m.Discover()
		assert.Equal(t, 1, conn.sent(), name)
		assert.Equal(t, 1, m.STUNChecks(), name)
	}
}

// Guard mode keeps public STUN servers out entirely: only peers are asked.
func TestSTUNCanBeForbidden(t *testing.T) {
	conn := newSTUNCounter()
	m := NewNATManager(conn, quiet())
	m.SetSTUNServers([]string{"127.0.0.1:3478"})
	m.SetSTUNAllowed(func() bool { return false })
	m.SetObserver(seen("server1", "203.0.113.7:50000"))
	_, err := m.Discover()
	require.NoError(t, err, "one peer still gives an address")
	assert.Equal(t, "203.0.113.7:50000", m.GetPublicAddr())
	m.SetObserver(seen())
	_, _ = m.Discover()
	m.ping(context.Background())
	assert.Equal(t, 0, conn.sent())
}
