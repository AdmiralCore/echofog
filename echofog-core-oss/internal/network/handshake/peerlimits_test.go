package handshake

import (
	"net/netip"
	"testing"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func id(b ...byte) types.PeerID {
	var p types.PeerID
	copy(p[:], b)
	return p
}

func TestAdmitPeerLimitsPeersPerAddress(t *testing.T) {
	g := NewGuard(GuardConfig{MaxPeersPerIP: 2})
	ip := netip.MustParseAddr("203.0.113.1")
	r1, err := g.AdmitPeer(ip, id(1))
	require.NoError(t, err)
	_, err = g.AdmitPeer(ip, id(2))
	require.NoError(t, err)
	_, err = g.AdmitPeer(ip, id(3))
	assert.ErrorIs(t, err, ErrAddrFull)
	assert.True(t, g.Refuses(ip), "a full address is refused before its handshake")
	_, allowed := g.Allow(ip)
	assert.False(t, allowed)

	// the same peer reconnecting is always let back in
	again, err := g.AdmitPeer(ip, id(1))
	require.NoError(t, err)
	again()
	assert.Equal(t, 2, g.PeersFrom(ip), "a reconnect does not count twice")

	r1()
	r1() // idempotent
	assert.Equal(t, 1, g.PeersFrom(ip))
	_, err = g.AdmitPeer(ip, id(3))
	assert.NoError(t, err, "room again once a peer left")
}

func TestAdmitPeerLimitsSubnet(t *testing.T) {
	g := NewGuard(GuardConfig{MaxPeersPerIP: 10, MaxPeersPerSubnet: 3})
	for i := 1; i <= 3; i++ {
		_, err := g.AdmitPeer(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}), id(byte(i)))
		require.NoError(t, err)
	}
	_, err := g.AdmitPeer(netip.MustParseAddr("198.51.100.200"), id(9))
	assert.ErrorIs(t, err, ErrSubnetFull, "a fourth address of the same /24")
	_, err = g.AdmitPeer(netip.MustParseAddr("198.51.101.1"), id(9))
	assert.NoError(t, err, "the next /24 is another subnet")

	// IPv6: one /56 is one customer
	g6 := NewGuard(GuardConfig{MaxPeersPerIP: 10, MaxPeersPerSubnet: 1})
	_, err = g6.AdmitPeer(netip.MustParseAddr("2001:db8:0:1::1"), id(1))
	require.NoError(t, err)
	_, err = g6.AdmitPeer(netip.MustParseAddr("2001:db8:0:2::1"), id(2))
	assert.ErrorIs(t, err, ErrSubnetFull, "same /56")
	_, err = g6.AdmitPeer(netip.MustParseAddr("2001:db8:0:100::1"), id(3))
	assert.NoError(t, err, "another /56")
}

func TestAdmitPeerPrivateNetworksHaveNoSubnetLimit(t *testing.T) {
	g := NewGuard(GuardConfig{MaxPeersPerSubnet: 1, MaxIDsPerSubnet: 1})
	for i := 1; i <= 5; i++ {
		_, err := g.AdmitPeer(netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}), id(byte(i)))
		require.NoError(t, err, "a container network is one operator")
	}
}

// Peer IDs are free: one address may show only so many different ones within the window, even
// when each leaves before the next comes.
func TestAdmitPeerLimitsIdentitiesPerAddress(t *testing.T) {
	g := NewGuard(GuardConfig{MaxIDsPerIP: 3, IDWindow: time.Minute})
	now := time.Unix(1000, 0)
	g.SetClock(func() time.Time { return now })
	ip := netip.MustParseAddr("203.0.113.7")
	for i := 1; i <= 3; i++ {
		r, err := g.AdmitPeer(ip, id(byte(i)))
		require.NoError(t, err)
		r() // gone at once: still counted
	}
	_, err := g.AdmitPeer(ip, id(4))
	assert.ErrorIs(t, err, ErrTooManyIDs)
	_, err = g.AdmitPeer(ip, id(2))
	assert.NoError(t, err, "an identity seen before comes back")

	now = now.Add(2 * time.Minute)
	_, err = g.AdmitPeer(ip, id(5))
	assert.NoError(t, err, "the window has passed")
}

func TestAdmitPeerLimitsIdentitiesPerSubnet(t *testing.T) {
	g := NewGuard(GuardConfig{MaxIDsPerIP: 100, MaxIDsPerSubnet: 4})
	for i := 1; i <= 4; i++ {
		r, err := g.AdmitPeer(netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}), id(byte(i)))
		require.NoError(t, err)
		r()
	}
	_, err := g.AdmitPeer(netip.MustParseAddr("192.0.2.50"), id(50))
	assert.ErrorIs(t, err, ErrTooManyIDs, "rotating addresses inside one /24 does not help")
}

func TestBanTurnsAddressAway(t *testing.T) {
	g := NewGuard(GuardConfig{})
	now := time.Unix(1000, 0)
	g.SetClock(func() time.Time { return now })
	ip := netip.MustParseAddr("203.0.113.66")
	g.Ban(ip, time.Minute)
	assert.True(t, g.Banned(ip))
	assert.True(t, g.Refuses(ip))
	_, err := g.AdmitPeer(ip, id(1))
	assert.ErrorIs(t, err, ErrAddrBanned)
	now = now.Add(2 * time.Minute)
	assert.False(t, g.Refuses(ip))

	lo := netip.MustParseAddr("127.0.0.1")
	g.Ban(lo, time.Hour)
	assert.False(t, g.Banned(lo), "local connections are not limited by default")
}

func TestPeerLimitsCanBeTurnedOff(t *testing.T) {
	g := NewGuard(GuardConfig{})
	g.SetPeerLimits(-1, -1, -1, -1)
	ip := netip.MustParseAddr("203.0.113.8")
	for i := 0; i < 300; i++ {
		_, err := g.AdmitPeer(ip, id(byte(i), byte(i>>8)))
		require.NoError(t, err)
	}
}

func screened(t *testing.T, screen ScreenFunc) (*internal_pb.PuzzleChallenge, error) {
	t.Helper()
	c := cfg()
	c.AntiDDoS = NewAntiDDoS()
	c.Screen = screen
	return handshakeWith(t, c)
}

// A server asks strangers for a puzzle even when calm; a full node refuses them before it signs.
func TestScreenAsksPuzzleOrRefuses(t *testing.T) {
	ch, err := screened(t, func(types.PeerPublicKey) (uint32, bool) { return 16, false })
	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.EqualValues(t, 16, ch.Difficulty)

	ch, err = screened(t, func(types.PeerPublicKey) (uint32, bool) { return 0, false })
	require.NoError(t, err)
	assert.Nil(t, ch, "a known peer pays nothing")

	_, err = screened(t, func(types.PeerPublicKey) (uint32, bool) { return 0, true })
	assert.Error(t, err)
}

func TestScreenRefusalIsErrPeerRefused(t *testing.T) {
	fa, fb := pipe()
	c := cfg()
	c.Screen = func(types.PeerPublicKey) (uint32, bool) { return 0, true }
	rb := run(NewNegotiator(newSigner(t), c, fb), binding(4), false)
	ra := run(NewNegotiator(newSigner(t), cfg(), fa), binding(4), true)
	r := <-rb
	assert.ErrorIs(t, r.err, ErrPeerRefused)
	assert.Error(t, (<-ra).err, "the initiator hears nothing and gives up")
}

// The puzzle a server asks of strangers must stay cheap for a real client: measure it.
func TestStrangerPuzzleLatency(t *testing.T) {
	const runs = 20
	var total, worst time.Duration
	for i := 0; i < runs; i++ {
		prefix := make([]byte, 16)
		prefix[0], prefix[1] = byte(i), byte(i>>8)
		start := time.Now()
		n := SolvePuzzle(prefix, StrangerPoWBits)
		d := time.Since(start)
		require.True(t, VerifyPuzzle(prefix, StrangerPoWBits, n))
		total += d
		if d > worst {
			worst = d
		}
	}
	t.Logf("stranger puzzle (%d bits): mean %v, worst %v over %d runs", StrangerPoWBits, total/runs, worst, runs)
	assert.Less(t, total/runs, 250*time.Millisecond, "a stranger's puzzle must stay well under a round trip on a slow phone")
}

func BenchmarkSolveStrangerPuzzle(b *testing.B) {
	prefix := make([]byte, 16)
	for i := 0; i < b.N; i++ {
		prefix[0], prefix[1], prefix[2] = byte(i), byte(i>>8), byte(i>>16)
		SolvePuzzle(prefix, StrangerPoWBits)
	}
}
