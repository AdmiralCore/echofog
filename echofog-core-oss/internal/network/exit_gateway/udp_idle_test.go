package exit_gateway

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func shortUDPTimers(t *testing.T, dns, evict time.Duration) {
	before := udpTimers.Load()
	udpTimers.Store(&udpTimerSet{idle: time.Hour, dnsIdle: dns, check: 20 * time.Millisecond, evict: evict})
	t.Cleanup(func() { udpTimers.Store(before) })
}

// A session that only asked DNS ends soon after; one that carries anything else stays open.
func TestExitUDPDNSOnlySessionEndsSoon(t *testing.T) {
	shortUDPTimers(t, 200*time.Millisecond, time.Hour)
	echo := udpEchoServer(t).LocalAddr().String()
	svc := openExit()
	open := serveUDP(svc, types.PeerID{1})

	dns, err := NewP2PDialer(open).DialExitUDP(context.Background())
	require.NoError(t, err)
	defer dns.Close()
	require.NoError(t, dns.WriteTo([]byte("query"), "127.0.0.1:53"))

	other, err := NewP2PDialer(open).DialExitUDP(context.Background())
	require.NoError(t, err)
	defer other.Close()
	require.NoError(t, other.WriteTo([]byte("game"), echo))
	readWithin(t, other, 2*time.Second)

	require.Eventually(t, func() bool { return svc.Streams() == 1 }, 2*time.Second, 10*time.Millisecond,
		"the DNS session is closed, the other one is not")
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, svc.Streams())
}

// A peer out of UDP sessions gets a new one in place of its stalest, not a refusal; a session
// that is in use is not taken away.
func TestExitUDPReplacesTheStalestSession(t *testing.T) {
	shortUDPTimers(t, time.Hour, 150*time.Millisecond)
	echo := udpEchoServer(t).LocalAddr().String()
	svc := openExit()
	l := DefaultLimits()
	l.UDPPerPeer = 1
	svc.SetLimits(l)
	open := serveUDP(svc, types.PeerID{1})

	old, err := NewP2PDialer(open).DialExitUDP(context.Background())
	require.NoError(t, err)
	defer old.Close()
	require.NoError(t, old.WriteTo([]byte("a"), echo))
	readWithin(t, old, 2*time.Second)

	_, err = NewP2PDialer(open).DialExitUDP(context.Background())
	assert.ErrorIs(t, err, ErrExitBusy, "the only session was just used")

	time.Sleep(250 * time.Millisecond)
	fresh, err := NewP2PDialer(open).DialExitUDP(context.Background())
	require.NoError(t, err, "the stale session made room")
	defer fresh.Close()
	require.NoError(t, fresh.WriteTo([]byte("b"), echo))
	got, _ := readWithin(t, fresh, 2*time.Second)
	assert.Equal(t, "b", string(got))
	assert.Equal(t, 1, svc.Streams(), "the replaced session gave its slot back")
}
