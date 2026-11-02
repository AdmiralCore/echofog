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
	"github.com/pion/stun/v4"
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
	m := discover(t, seen("a", "8.8.8.7:50000", "b", "8.8.8.7:50000"))
	addr, typ := m.GetNATProfile()
	assert.Equal(t, "8.8.8.7:50000", addr)
	assert.Equal(t, NATFullCone, typ)
	assert.True(t, m.CanBePunched())
	assert.False(t, m.CanBeRouter(), "a cone NAT lets answers in, not strangers")
}

func TestSymmetricNATCannotBePunched(t *testing.T) {
	m := discover(t, seen("a", "8.8.8.7:50000", "b", "8.8.8.7:50777", "c", "8.8.8.7:50555"))
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATSymmetric, typ)
	assert.False(t, m.CanBePunched())
	assert.False(t, m.CanBeRouter())
}

// A symmetric NAT whose router forwards our port: that port is one public address for everybody,
// so peers reach us there (it was taken for unreachable, and nothing was even tried).
func TestForwardedPortBeatsASymmetricNAT(t *testing.T) {
	m := discover(t, seen("a", "8.8.8.7:50000", "b", "8.8.8.7:50777", "c", "8.8.8.7:50555"))
	m.SetPortMapping(netip.MustParseAddrPort("8.8.8.7:49829"))
	assert.True(t, m.CanBePunched())
	assert.True(t, m.CanBeRouter())
	assert.Equal(t, "8.8.8.7:49829", m.Candidates()[0])
}

func TestOneObserverIsWorthATry(t *testing.T) {
	m := discover(t, seen("a", "8.8.8.7:50000"))
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATUnknown, typ)
	assert.True(t, m.CanBePunched(), "unknown is not a reason to give up")
	assert.False(t, m.CanBeRouter())
}

func TestAForwardedPortMakesUsReachableEvenBehindNAT(t *testing.T) {
	m := discover(t, seen("a", "8.8.8.7:50000", "b", "8.8.8.7:50000"))
	require.False(t, m.CanBeRouter())

	m.SetPortMapping(netip.MustParseAddrPort("8.8.8.7:4242"))
	assert.True(t, m.CanBeRouter())
	assert.Equal(t, []string{"8.8.8.7:4242", "8.8.8.7:50000"}, m.Candidates(), "the forwarded address first")

	m.SetPortMapping(netip.AddrPort{})
	assert.False(t, m.CanBeRouter(), "the router dropped the forward")
}

func TestCandidatesDoNotRepeatTheSameAddress(t *testing.T) {
	m := discover(t, seen("a", "8.8.8.7:4242", "b", "8.8.8.7:4242"))
	m.SetPortMapping(netip.MustParseAddrPort("8.8.8.7:4242"))
	assert.Equal(t, []string{"8.8.8.7:4242"}, m.Candidates())
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
	obs := seen("a", "8.8.8.7:50000", "b", "8.8.8.7:50000")
	m := NewNATManager(nil, quiet())
	m.SetObserver(func(ctx context.Context) []natcheck.Observation { return obs(ctx) })
	_, _ = m.Discover()
	_, typ := m.GetNATProfile()
	require.Equal(t, NATFullCone, typ)

	obs = seen("a", "9.9.9.1:51000", "b", "9.9.9.1:51003", "c", "9.9.9.1:51009") // moved to a carrier NAT
	_, _ = m.Discover()
	addr, typ := m.GetNATProfile()
	assert.Equal(t, NATSymmetric, typ)
	assert.Equal(t, "9.9.9.1", netip.MustParseAddrPort(addr).Addr().String())
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

// Three connected peers that agree tell the public address and the NAT kind: no request goes to a
// public STUN server, and no STUN keep-alive either (the peers' QUIC keep-alives hold the mapping
// open).
func TestPeersReplaceSTUNWhenThreeOfThemAgree(t *testing.T) {
	conn := newSTUNCounter()
	m := NewNATManager(conn, quiet())
	m.SetSTUNServers([]string{"127.0.0.1:3478"})
	m.SetObserver(seen("server1", "8.8.8.7:50000", "server2", "8.8.8.7:50000", "server3", "8.8.8.7:50000"))
	_, err := m.Discover()
	require.NoError(t, err)
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATFullCone, typ, "learned from the peers alone")
	assert.Equal(t, 0, conn.sent(), "no STUN request when enough peers agreed")
	assert.Equal(t, 0, m.STUNChecks())

	m.ping(context.Background())
	assert.Equal(t, 0, conn.sent(), "connected peers keep the mapping open: no STUN keep-alive")
}

func TestPortRangeFromPeers(t *testing.T) {
	m := discover(t, seen("server1", "8.8.8.7:50000", "server2", "8.8.8.7:50777", "server3", "8.8.8.7:50400"))
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATSymmetric, typ)
	lo, hi, n := m.PortRange()
	assert.Equal(t, uint16(50000), lo)
	assert.Equal(t, uint16(50777), hi)
	assert.Equal(t, 3, n)
}

// With too few peers (just started, one or two) or peers that do not agree, STUN is asked as
// well; peers on our LAN do not count, their view is from inside the NAT. Two peers are not enough:
// both slots are cheap to take (a peer only has to be connected), and two liars agreeing used to
// keep STUN out entirely.
func TestSTUNIsAskedWhenTooFewPeersAgree(t *testing.T) {
	for name, o := range map[string]Observer{
		"no peer":         seen(),
		"one peer":        seen("server1", "8.8.8.7:50000"),
		"only on LAN":     seen("lan1", "192.168.1.5:4242", "lan2", "192.168.1.5:4242"),
		"one observer":    seen("server1", "8.8.8.7:50000", "server1", "8.8.8.7:50000"),
		"two agree":       seen("server1", "8.8.8.7:50000", "server2", "8.8.8.7:50000"),
		"one of two lies": seen("server1", "8.8.8.7:50000", "liar", "2.16.0.1:1"),
		"three, a doubt":  seen("a", "8.8.8.7:50000", "b", "8.8.8.7:50000", "liar", "2.16.0.1:1"),
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

// fakeSTUNServer answers every binding request with mapped: a STUN server that sees us there.
func fakeSTUNServer(t *testing.T, mapped netip.AddrPort) string {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var req stun.Message
			req.Raw = append([]byte(nil), buf[:n]...)
			if req.Decode() != nil || req.Type != stun.BindingRequest {
				continue
			}
			resp, err := stun.Build(stun.NewTransactionIDSetter(req.TransactionID), stun.BindingSuccess,
				&stun.XORMappedAddress{IP: mapped.Addr().AsSlice(), Port: int(mapped.Port())}, stun.Fingerprint)
			if err == nil {
				_, _ = pc.WriteTo(resp.Raw, from)
			}
		}
	}()
	return pc.LocalAddr().String()
}

// One lying peer out of two used to choose our address (a tie went to the lower address as a
// string, and "2.16.0.1" sorts before most addresses) and make the NAT look symmetric. Now the tie
// sends the node to STUN, whose answer outvotes the liar.
func TestALyingPeerOutOfTwoDoesNotChooseOurAddress(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()
	m := NewNATManager(conn, quiet())
	m.SetSTUNServers([]string{fakeSTUNServer(t, netip.MustParseAddrPort("9.9.9.9:41000"))})
	m.SetObserver(seen("honest", "9.9.9.9:41000", "liar", "2.16.0.1:1"))
	_, err = m.Discover()
	require.NoError(t, err)
	addr, typ := m.GetNATProfile()
	assert.Equal(t, "9.9.9.9:41000", addr)
	assert.Equal(t, NATFullCone, typ, "one disagreeing peer does not make a symmetric NAT")
	assert.Equal(t, 1, m.STUNChecks())

	// without STUN to break the tie, the tie decides nothing
	alone := discover(t, seen("honest", "9.9.9.9:41000", "liar", "2.16.0.1:1"))
	addr, typ = alone.GetNATProfile()
	assert.Empty(t, addr)
	assert.NotEqual(t, NATSymmetric, typ)
}

// Guard mode keeps public STUN servers out entirely: only peers are asked, and one peer alone
// cannot set the address (it would place the node in a country, and a free country stands guard
// mode down). Two agreeing peers can; a later lone peer saying otherwise changes nothing.
func TestSTUNCanBeForbidden(t *testing.T) {
	conn := newSTUNCounter()
	m := NewNATManager(conn, quiet())
	m.SetSTUNServers([]string{"127.0.0.1:3478"})
	m.SetSTUNAllowed(func() bool { return false })
	m.SetObserver(seen("liar", "2.16.0.1:1"))
	_, err := m.Discover()
	require.NoError(t, err)
	assert.Empty(t, m.GetPublicAddr(), "one peer alone does not place the node")

	m.SetObserver(seen("guard1", "8.8.8.7:50000", "guard2", "8.8.8.7:50000"))
	_, _ = m.Discover()
	assert.Equal(t, "8.8.8.7:50000", m.GetPublicAddr(), "two agreeing peers do")

	m.SetObserver(seen("liar", "2.16.0.1:1"))
	_, _ = m.Discover()
	assert.Equal(t, "8.8.8.7:50000", m.GetPublicAddr(), "a lone peer does not move it")
	m.SetObserver(seen("guard1", "8.8.8.7:50000", "liar", "2.16.0.1:1"))
	_, _ = m.Discover()
	assert.Equal(t, "8.8.8.7:50000", m.GetPublicAddr(), "nor does a tie")

	m.SetObserver(seen())
	_, _ = m.Discover()
	m.ping(context.Background())
	assert.Equal(t, 0, conn.sent())
}

// In guard mode the guards are reached over REALITY, each dial on a socket of its own, and they
// may be the only peers. Their answers place the node (the IP, for its country) without telling
// the port or the mapping: guard_mode "auto" used to wait forever for a country.
func TestPeersOnRealitySocketsTellTheIPForTheCountry(t *testing.T) {
	conn := newSTUNCounter()
	m := NewNATManager(conn, quiet())
	m.SetSTUNAllowed(func() bool { return false }) // guard mode
	m.SetObserver(func(context.Context) []natcheck.Observation {
		return []natcheck.Observation{
			{From: "guard1", Addr: netip.MustParseAddrPort("8.8.8.7:61001"), IPOnly: true},
			{From: "guard2", Addr: netip.MustParseAddrPort("8.8.8.7:61877"), IPOnly: true},
		}
	})
	_, err := m.Discover()
	require.NoError(t, err)
	assert.Equal(t, "8.8.8.7", m.PublicIP(), "the IP places the node")
	assert.Empty(t, m.GetPublicAddr(), "no port: those sockets have mappings of their own")
	assert.Empty(t, m.Candidates())
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATUnknown, typ, "different ports on different sockets are not a symmetric NAT")
	assert.Equal(t, 0, conn.sent())

	// one guard alone is not enough
	lone := NewNATManager(conn, quiet())
	lone.SetSTUNAllowed(func() bool { return false })
	lone.SetObserver(func(context.Context) []natcheck.Observation {
		return []natcheck.Observation{{From: "guard1", Addr: netip.MustParseAddrPort("8.8.8.7:61001"), IPOnly: true}}
	})
	_, _ = lone.Discover()
	assert.Empty(t, lone.PublicIP())
}

// Peers that answer over IPv6 do not count toward the IPv4 observers: three of them used to keep
// STUN out, and the IPv4 NAT was never classified (nor the IPv4 address known for invitations).
func TestIPv6PeersDoNotReplaceSTUNForIPv4(t *testing.T) {
	conn := newSTUNCounter()
	m := NewNATManager(conn, quiet())
	m.SetSTUNServers([]string{"127.0.0.1:3478"})
	m.SetObserver(seen("a", "[2001:4860:4860::8888]:4242", "b", "[2001:4860:4860::8888]:4242",
		"c", "[2001:4860:4860::8888]:4242"))
	_, _ = m.Discover()
	assert.Equal(t, 1, conn.sent(), "STUN tells the IPv4 side")

	// one IPv4 peer next to them is still too few
	conn = newSTUNCounter()
	m = NewNATManager(conn, quiet())
	m.SetSTUNServers([]string{"127.0.0.1:3478"})
	m.SetObserver(seen("a", "[2001:4860:4860::8888]:4242", "b", "[2001:4860:4860::8888]:4242",
		"c", "8.8.8.7:50000"))
	_, _ = m.Discover()
	assert.Equal(t, 1, conn.sent())
}
