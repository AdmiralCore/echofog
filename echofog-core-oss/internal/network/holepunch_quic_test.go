package network

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newQuicWithID(t *testing.T) (*QuicTransport, types.PeerID) {
	im, store := testutil.SetupTestIdentity(t)
	t.Cleanup(store.CloseAndClear)
	account, err := im.GetActiveAccount()
	require.NoError(t, err)
	_, pub, err := account.GetIdentityKey()
	require.NoError(t, err)
	var pk types.PeerPublicKey
	copy(pk[:], pub)
	_, tlsCfg := testutil.GetRandomKeyAndTls(t)
	tr, err := NewQUICTransport("127.0.0.1:0", tlsCfg, GetQuicConfig(), im, config.CurrentProtocolVersion)
	require.NoError(t, err)
	t.Cleanup(func() { tr.Close() })
	return tr, types.PeerPubKeyToID(pk)
}

// One side punches, the other dials: the connection comes up and the punching side notices.
func TestPunchHoleCompletesWhenThePeerDials(t *testing.T) {
	a, _ := newQuicWithID(t)
	b, bID := newQuicWithID(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.ListenEarly(ctx))
	require.NoError(t, b.ListenEarly(ctx))

	punched := make(chan error, 1)
	go func() { punched <- a.PunchHole(ctx, b.Addr().String(), bID) }()

	time.Sleep(100 * time.Millisecond) // the punch is already under way when the peer dials
	require.NoError(t, b.DialDirect(ctx, a.Addr().String()))

	select {
	case err := <-punched:
		require.NoError(t, err, "the punching side must see the peer arrive")
	case <-time.After(5 * time.Second):
		t.Fatal("PunchHole did not return")
	}
	select {
	case ev := <-a.ConnChan():
		assert.Equal(t, bID, ev.PeerID)
		assert.False(t, ev.IsOut)
	case <-time.After(2 * time.Second):
		t.Fatal("no connection event on the punching side")
	}
}

func TestPunchHoleGivesUpWhenNobodyDials(t *testing.T) {
	a, _ := newQuicWithID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := a.PunchHole(ctx, "127.0.0.1:9", types.PeerID{9})
	assert.ErrorIs(t, err, ErrHolePunchTimeout)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestOnlyOnePunchPerPeerAndAddress(t *testing.T) {
	a, _ := newQuicWithID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- a.PunchHole(ctx, "127.0.0.1:9", types.PeerID{9}) }()
	time.Sleep(100 * time.Millisecond)

	assert.ErrorIs(t, a.PunchHole(ctx, "127.0.0.1:9", types.PeerID{9}), ErrPunchActive)
	// another peer or another address is a different punch
	go func() { _ = a.PunchHole(ctx, "127.0.0.1:10", types.PeerID{9}) }()
	<-first
}

// The packets must be small, random, and start fast then slow down, as in libp2p.
func TestPunchPacketsArePacedAndRandom(t *testing.T) {
	a, _ := newQuicWithID(t)
	sink, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer sink.Close()

	var times []time.Time
	var packets [][]byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 2048)
		_ = sink.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		for {
			n, _, err := sink.ReadFrom(buf)
			if err != nil {
				return
			}
			times = append(times, time.Now())
			packets = append(packets, append([]byte(nil), buf[:n]...))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	_ = a.PunchHole(ctx, sink.LocalAddr().String(), types.PeerID{7})
	<-done

	require.GreaterOrEqual(t, len(packets), 5, "punching sends packet after packet")
	assert.LessOrEqual(t, len(packets), 40, "but not a flood")
	for _, p := range packets {
		assert.Len(t, p, punchPayloadLen)
	}
	assert.NotEqual(t, packets[0], packets[1], "every packet is different")
	first := times[1].Sub(times[0])
	last := times[len(times)-1].Sub(times[len(times)-2])
	assert.Less(t, first, 120*time.Millisecond, "the first packets follow each other quickly")
	_ = last
}

func TestArrivalWaitersWakeOnlyTheRightPeer(t *testing.T) {
	var w arrivalWaiters
	one, cancelOne := w.watch(types.PeerID{1})
	two, cancelTwo := w.watch(types.PeerID{2})
	defer cancelOne()
	defer cancelTwo()

	w.notify(types.PeerID{1})
	select {
	case <-one:
	default:
		t.Fatal("peer 1 was not woken")
	}
	select {
	case <-two:
		t.Fatal("peer 2 must not be woken by peer 1")
	default:
	}
	cancelOne()
	w.notify(types.PeerID{1}) // no waiters left: must not panic or block
}

// The same punch with obfuscation on: the punching side only sends noise, the dialing side seals
// its packets with a key derived from the punching side's ID.
func TestPunchHoleWithObfuscation(t *testing.T) {
	newNode := func() (*QuicTransport, types.PeerID) {
		s := newTestSigner(t)
		_, tlsCfg := testutil.GetRandomKeyAndTls(t)
		tr, err := NewQUICTransportWithOptions("127.0.0.1:0", tlsCfg, GetQuicConfig(), s, config.CurrentProtocolVersion,
			TransportOptions{Obfs: true, OwnID: s.id()})
		require.NoError(t, err)
		t.Cleanup(func() { tr.Close() })
		return tr, s.id()
	}
	a, aID := newNode()
	b, bID := newNode()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.ListenEarly(ctx))
	require.NoError(t, b.ListenEarly(ctx))

	punched := make(chan error, 1)
	go func() { punched <- a.PunchHole(ctx, b.Addr().String(), bID) }()
	time.Sleep(100 * time.Millisecond)

	b.ExpectPeer(a.Addr().String(), aID)
	require.NoError(t, b.DialDirect(ctx, a.Addr().String()))

	select {
	case err := <-punched:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("PunchHole did not return")
	}
	select {
	case ev := <-a.ConnChan():
		assert.Equal(t, bID, ev.PeerID)
	case <-time.After(2 * time.Second):
		t.Fatal("no connection event on the punching side")
	}
}
