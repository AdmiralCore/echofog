package exit_gateway

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExitLimitsFromConfig(t *testing.T) {
	d := DefaultLimits()
	assert.Equal(t, defaultExitStreamsPerPeer, d.PerPeer)
	assert.Equal(t, defaultExitIdle, d.Idle)
	assert.Zero(t, d.MaxLifetime, "no lifetime cap unless asked")
	l := LimitsFromConfig(config.ExitConfig{MaxStreamsPerPeer: -1, IdleSeconds: -1, MaxLifetimeSeconds: 60, FreePeerMbps: 8})
	assert.Zero(t, l.PerPeer, "negative: no limit")
	assert.Zero(t, l.Idle)
	assert.Equal(t, time.Minute, l.MaxLifetime)
	assert.Equal(t, 1e6, l.FreePeerRate)
}

// One peer at its stream cap is told the exit is busy; a slot comes back when a stream ends.
func TestExitStreamCapPerPeer(t *testing.T) {
	target := echoServer(t)
	svc := NewExitService()
	svc.allowPrivate.Store(true)
	svc.SetEnabled(true)
	l := DefaultLimits()
	l.PerPeer = 1
	svc.SetLimits(l)
	d := NewP2PDialer(serve(svc))

	first, err := d.DialExitStreamConfirmed(context.Background(), target)
	require.NoError(t, err)
	_, err = d.DialExitStreamConfirmed(context.Background(), target)
	assert.ErrorIs(t, err, ErrExitBusy)
	assert.Equal(t, "busy", ExitRefusal(err))
	assert.Equal(t, uint64(1), svc.RefusedByReason()[RefuseBusy])

	first.Close()
	require.Eventually(t, func() bool { return svc.Streams() == 0 }, 2*time.Second, 10*time.Millisecond)
	again, err := d.DialExitStreamConfirmed(context.Background(), target)
	require.NoError(t, err)
	again.Close()
}

// Many identities behind one public address share its cap; a LAN address is not limited.
func TestExitStreamCapPerIPAndUDP(t *testing.T) {
	svc := NewExitService()
	l := DefaultLimits()
	l.PerIP, l.UDPPerPeer = 2, 1
	svc.SetLimits(l)
	addr := map[types.PeerID]string{}
	svc.SetAddrOf(func(id types.PeerID) string { return addr[id] })
	a, b, c, lan := types.PeerID{1}, types.PeerID{2}, types.PeerID{3}, types.PeerID{4}
	addr[a], addr[b], addr[c], addr[lan] = "198.51.100.4:1", "198.51.100.4:2", "198.51.100.4:3", "192.168.1.9:5"

	ra, ok := svc.acquire(a, false)
	require.True(t, ok)
	_, ok = svc.acquire(b, true)
	require.True(t, ok)
	_, ok = svc.acquire(c, false)
	assert.False(t, ok, "a third identity from the same IP")
	for i := 0; i < 5; i++ {
		_, ok = svc.acquire(lan, false)
		assert.True(t, ok, "LAN peers are not limited per IP")
	}
	_, ok = svc.acquire(b, true)
	assert.False(t, ok, "a second UDP session over the per-peer cap")

	ra()
	ra() // releasing twice is harmless
	_, ok = svc.acquire(c, false)
	assert.True(t, ok, "a released slot is free again")
	assert.Equal(t, 7, svc.Streams())
}

func TestExitNodeWideStreamCap(t *testing.T) {
	svc := NewExitService()
	svc.SetLimits(Limits{MaxStreams: 3})
	for i := 0; i < 3; i++ {
		_, ok := svc.acquire(types.PeerID{byte(i)}, false)
		require.True(t, ok)
	}
	_, ok := svc.acquire(types.PeerID{9}, false)
	assert.False(t, ok)
}

// A tunnel that moves nothing is closed after the idle timeout, both sides.
func TestExitClosesIdleTunnel(t *testing.T) {
	target := echoServer(t)
	svc := NewExitService()
	svc.allowPrivate.Store(true)
	svc.SetEnabled(true)
	svc.SetLimits(Limits{Idle: 200 * time.Millisecond})

	conn, err := NewP2PDialer(serve(svc)).DialExitStream(context.Background(), target)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	conn.(deadliner).SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)

	start := time.Now()
	_, err = conn.Read(buf)
	assert.Error(t, err, "the exit closed the idle tunnel")
	assert.Less(t, time.Since(start), 1500*time.Millisecond)
	require.Eventually(t, func() bool { return svc.Streams() == 0 }, 2*time.Second, 10*time.Millisecond)
}

// A busy tunnel still ends at the lifetime cap.
func TestExitTunnelLifetimeCap(t *testing.T) {
	target := echoServer(t)
	svc := NewExitService()
	svc.allowPrivate.Store(true)
	svc.SetEnabled(true)
	svc.SetLimits(Limits{MaxLifetime: 300 * time.Millisecond})

	conn, err := NewP2PDialer(serve(svc)).DialExitStream(context.Background(), target)
	require.NoError(t, err)
	defer conn.Close()
	conn.(deadliner).SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	buf := make([]byte, 4)
	for {
		if _, err := conn.Write([]byte("busy")); err != nil {
			break
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond)
}

// The free quota table stays bounded without handing everybody their quota back.
func TestExitQuotaTableBounded(t *testing.T) {
	svc := NewExitService()
	svc.SetPolicy(Policy{PricePerMB: 50, FreeBytes: 1 << 20})
	today := time.Now().UTC().Truncate(24 * time.Hour)
	heavy := types.PeerID{0xFF, 0xFF}
	svc.policyMu.Lock()
	for i := 0; i < quotaPeers; i++ {
		svc.quotas[types.PeerID{byte(i), byte(i >> 8), 1}] = &peerQuota{used: int64(i + 1), day: today}
	}
	svc.quotas[heavy] = &peerQuota{used: 1 << 20, day: today}
	svc.quotaLocked(types.PeerID{0xAB, 0xCD, 0xEF})
	n, kept := len(svc.quotas), svc.quotas[heavy] != nil
	svc.policyMu.Unlock()
	assert.LessOrEqual(t, n, quotaPeers)
	assert.Greater(t, n, quotaPeers/4, "only part of the table is dropped")
	assert.True(t, kept, "the peer that used its quota is remembered")
	assert.Zero(t, svc.allowance(heavy))
}

// A free exit gives each peer, and each address, its own share; its own devices and paying
// peers are not slowed down.
func TestFreeExitFairShare(t *testing.T) {
	svc := NewExitService()
	svc.SetLimits(Limits{FreePeerRate: 1e6, FreeIPRate: 1.5e6})
	addr := map[types.PeerID]string{}
	svc.SetAddrOf(func(id types.PeerID) string { return addr[id] })
	a, b, mine := types.PeerID{1}, types.PeerID{2}, types.PeerID{3}
	addr[a], addr[b] = "203.0.113.8:1", "203.0.113.8:2"
	svc.SetFreePeers([]types.PeerID{mine})

	pa, relA := svc.paceFor(a)
	pb, relB := svc.paceFor(b)
	pa2, relA2 := svc.paceFor(a)
	require.NotNil(t, pa)
	assert.Same(t, pa.peer, pa2.peer, "one peer's streams share its bucket")
	assert.NotSame(t, pa.peer, pb.peer)
	assert.Same(t, pa.ip, pb.ip, "identities of one address share its bucket")

	assert.Zero(t, pa.wait(int(paceBurst(1e6))), "the burst passes at once")
	w := pa.wait(1e6)
	assert.InDelta(t, time.Second.Seconds(), w.Seconds(), 0.3, "past the burst, 1 MB waits about a second")

	p, rel := svc.paceFor(mine)
	assert.Nil(t, p, "the operator's own devices are not shaped")
	rel()
	svc.SetPolicy(Policy{PricePerMB: 50})
	p, rel = svc.paceFor(b)
	assert.Nil(t, p, "a paying exit does not shape")
	rel()

	relA()
	relA2()
	relB()
	svc.shaper.mu.Lock()
	assert.Empty(t, svc.shaper.peers)
	assert.Empty(t, svc.shaper.ips)
	svc.shaper.mu.Unlock()
}

func TestExitAccountQueriesRateLimited(t *testing.T) {
	svc := NewExitService()
	p := types.PeerID{7}
	for i := 0; i < queryBurst; i++ {
		require.True(t, svc.AllowQuery(p))
	}
	assert.False(t, svc.AllowQuery(p))
	assert.True(t, svc.AllowQuery(types.PeerID{8}), "another peer is not affected")
}

// Vouchers from made-up vaults cost chain reads: they are limited per peer, and an empty vault is
// not read again for a while, nor written into the book.
func TestVoucherLookupsOfUnknownVaultsLimited(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	now := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return now }
	ctx := context.Background()

	ghost := newTestUser(t, 50, chain, 0)
	assert.Contains(t, b.Accept(ctx, "x", ghost.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000)).Error, "empty")
	assert.Empty(t, b.data.Accounts, "an empty vault opens no account")

	// funded a moment later: the empty answer is remembered for a while...
	funded := newTestUser(t, 50, chain, 1_000_000_000)
	assert.Contains(t, b.Accept(ctx, "y", funded.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000)).Error, "empty")
	// ...then the vault is read again
	now = now.Add(vaultEmptyTTL + time.Second)
	assert.True(t, b.Accept(ctx, "y", funded.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000)).OK)

	// one peer making up vaults runs out of lookups
	var last VoucherReply
	for i := 0; i < vaultLookupBurst+1; i++ {
		last = b.Accept(ctx, "z", newTestUser(t, byte(60+i), chain, 0).offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000))
	}
	assert.Contains(t, last.Error, "too many vault checks")
	// a known vault is not affected
	assert.True(t, b.Accept(ctx, "y", funded.offer(t, tonwallet.MainnetGlobalID, exitPayout, 2000)).OK)
}
