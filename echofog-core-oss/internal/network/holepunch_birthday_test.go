package network

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newObfsQuicWithID is newQuicWithID with the per-node UDP obfuscation on, so the birthday sockets
// exercise the same sealing as every other datagram.
func newObfsQuicWithID(t *testing.T) (*QuicTransport, types.PeerID) {
	t.Helper()
	im, store := testutil.SetupTestIdentity(t)
	t.Cleanup(store.CloseAndClear)
	account, err := im.GetActiveAccount()
	require.NoError(t, err)
	_, pub, err := account.GetIdentityKey()
	require.NoError(t, err)
	var pk types.PeerPublicKey
	copy(pk[:], pub)
	id := types.PeerPubKeyToID(pk)
	_, tlsCfg := testutil.GetRandomKeyAndTls(t)
	tr, err := NewQUICTransportWithOptions("127.0.0.1:0", tlsCfg, GetQuicConfig(), im, config.CurrentProtocolVersion, TransportOptions{
		Obfs:  true,
		OwnID: id,
	})
	require.NoError(t, err)
	t.Cleanup(func() { tr.Close() })
	return tr, id
}

// A narrow block is swept, widened by half the block on each side, and every port stays inside that
// widened window; a wide or unknown range goes random over the dynamic range.
func TestBirthdayPorts(t *testing.T) {
	t.Run("narrow block is swept, widened well past the samples", func(t *testing.T) {
		const lo, hi = uint16(40000), uint16(44000)
		ports := birthdayPorts(lo, hi, 500)
		require.Len(t, ports, 500)
		base, top := int(lo)-birthdaySprayMargin, int(hi)+birthdaySprayMargin
		min, max := 65536, 0
		for _, p := range ports {
			assert.GreaterOrEqual(t, int(p), base)
			assert.LessOrEqual(t, int(p), top)
			assert.GreaterOrEqual(t, int(p), 1024)
			if int(p) < min {
				min = int(p)
			}
			if int(p) > max {
				max = int(p)
			}
		}
		// the spray reaches below the observed low and above the observed high (the samples are only
		// part of the NAT's real block)
		assert.Less(t, min, int(lo), "sprays below the observed range")
		assert.Greater(t, max, int(hi), "sprays above the observed range")
	})
	t.Run("one observed port still sweeps a window around it", func(t *testing.T) {
		ports := birthdayPorts(50000, 50000, 200)
		require.Len(t, ports, 200)
		for _, p := range ports {
			assert.GreaterOrEqual(t, int(p), 50000-birthdaySprayMargin)
			assert.LessOrEqual(t, int(p), 50000+birthdaySprayMargin)
		}
	})
	t.Run("wide range goes random over the dynamic range", func(t *testing.T) {
		ports := birthdayPorts(1100, 60000, 300)
		require.Len(t, ports, 300)
		for _, p := range ports {
			assert.GreaterOrEqual(t, int(p), 1024)
		}
	})
	t.Run("unknown range goes random over the dynamic range", func(t *testing.T) {
		ports := birthdayPorts(0, 0, 100)
		require.Len(t, ports, 100)
		for _, p := range ports {
			assert.GreaterOrEqual(t, int(p), 1024)
		}
	})
}

// Every socket opened by the symmetric side gets a packet in the first round of the shared pacer.
// A per-socket interval (or a rate multiplied by the socket count) leaves all but the first few
// sockets without a NAT mapping inside the window, which is the bug this test guards.
func TestBirthdayPaceLoopReachesEverySocket(t *testing.T) {
	const n = 64
	var seen [n]atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		birthdayPaceLoop(ctx, stop, n, func(i int, _ []byte) { seen[i].Add(1) })
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		missing := 0
		for i := range seen {
			if seen[i].Load() == 0 {
				missing++
			}
		}
		if missing == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d sockets never got a packet", missing, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	<-done

	// the round-robin must not favour the first sockets: after the first round each has one, and
	// the loop keeps going until stopped
	for i := range seen {
		assert.GreaterOrEqual(t, seen[i].Load(), int32(1))
	}
}

// The spray budget fits inside the punch window at the target rate: all 900 packets go out before
// the 3 s deadline, paced rather than in a burst. A one-sided jitter used to deliver only ~600.
func TestBirthdaySprayBudgetFitsTheWindow(t *testing.T) {
	q := &QuicTransport{}
	ports := make([]uint16, maxBirthdaySpray)
	for i := range ports {
		ports[i] = uint16(40000 + i%2) // any ports; the sender is a counter here
	}
	var times []time.Time
	ctx, cancel := context.WithTimeout(context.Background(), birthdayTimeout)
	defer cancel()
	err := q.birthdaySprayPorts(ctx, ports, func(uint16) error {
		times = append(times, time.Now())
		return nil
	}, nil)
	assert.ErrorIs(t, err, ErrHolePunchTimeout, "a full sweep then waits out the window")
	require.Len(t, times, maxBirthdaySpray, "the whole budget went out before the deadline")
	span := times[len(times)-1].Sub(times[0])
	assert.GreaterOrEqual(t, span, 2500*time.Millisecond, "paced, not a single burst")
	assert.LessOrEqual(t, span, 3*time.Second, "the last packet is inside the window")
}

// A real birthday punch with per-node obfuscation on: the cone side sprays guessed ports, the
// symmetric side's fresh sockets open the sealed packets, and the QUIC handshake crosses. This is
// the end-to-end guard for ExpectPeerIP: without it the spray is sealed with the wrong key and the
// symmetric side silently drops every packet.
func TestBirthdayPunchWithObfuscation(t *testing.T) {
	a, aID := newObfsQuicWithID(t)
	b, bID := newObfsQuicWithID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, a.ListenEarly(ctx))
	require.NoError(t, b.ListenEarly(ctx))

	var err error
	for attempt := 0; attempt < 3; attempt++ {
		sprayed := make(chan error, 1)
		go func() {
			// A random sweep of the whole dynamic range: the test cannot know the ephemeral ports
			// the dialer's fresh sockets got.
			sprayed <- b.BirthdaySpray(ctx, "127.0.0.1", 0, 0, maxBirthdaySpray, aID)
		}()
		err = a.BirthdayDial(ctx, b.Addr().String(), bID, maxBirthdaySockets)
		if err == nil {
			break
		}
		select {
		case <-sprayed:
		case <-time.After(4 * time.Second):
		}
	}
	require.NoError(t, err, "an obfuscated birthday punch reaches the cone peer")

	requireEvent := func(tr *QuicTransport, want types.PeerID) NewConnEvent {
		t.Helper()
		select {
		case ev := <-tr.ConnChan():
			assert.Equal(t, want, ev.PeerID)
			return ev
		case <-time.After(3 * time.Second):
			t.Fatalf("no authenticated connection for peer %x", want[:2])
			return NewConnEvent{}
		}
	}
	requireEvent(b, aID)
	requireEvent(a, bID)
}

// birthdayAccept is a guarded door like the main listener, but it also knows exactly which peer the
// punch was arranged with, and its address is an ephemeral mapping that must not be dialable later.
func TestBirthdayAcceptRejectsUnexpectedPeerAndMarksSessionUnaddressable(t *testing.T) {
	a, aID := newQuicWithID(t)
	b, bID := newQuicWithID(t)
	other, _ := newQuicWithID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pconn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)
	tr := &quic.Transport{Conn: pconn}
	ln, err := tr.ListenEarly(b.tlsCfg, b.quicCgf)
	require.NoError(t, err)
	go b.birthdayAccept(ctx, ln, tr, pconn, aID)

	target := net.UDPAddrFromAddrPort(netip.MustParseAddrPort(pconn.LocalAddr().String()))

	// The wrong peer authenticates its own identity and is hung up on; no connection event.
	otherConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)
	require.NoError(t, other.birthdayDialOn(ctx, otherConn, target, bID, true))
	select {
	case ev := <-b.ConnChan():
		t.Fatalf("unexpected peer %x got a connection", ev.PeerID[:2])
	case <-time.After(700 * time.Millisecond):
	}

	// The arranged peer gets in, but its ephemeral source address is not dialable.
	peerConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)
	require.NoError(t, a.birthdayDialOn(ctx, peerConn, target, bID, true))
	select {
	case ev := <-b.ConnChan():
		assert.Equal(t, aID, ev.PeerID)
		assert.False(t, ev.Dialable, "a birthday source port is a NAT mapping, not a listening port")
	case <-time.After(3 * time.Second):
		t.Fatal("the arranged peer never got a connection")
	}
}

// The spray is paced (it runs out the window rather than firing a single burst), bounded by the
// window, and made of small random datagrams. It now spreads across a port window (not one port),
// so a single sink sees only a sample; pacing and bounding are checked by time, datagram size by
// whatever the sink catches.
func TestBirthdaySprayPacedAndBounded(t *testing.T) {
	a, _ := newQuicWithID(t)
	ctx := context.Background()
	require.NoError(t, a.ListenEarly(ctx))

	sink, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer sink.Close()
	sinkPort := netip.MustParseAddrPort(sink.LocalAddr().String()).Port()
	go func() {
		buf := make([]byte, 2048)
		for {
			_ = sink.SetReadDeadline(time.Now().Add(4 * time.Second))
			n, _, e := sink.ReadFrom(buf)
			if e != nil {
				return
			}
			assert.Len(t, buf[:n], punchPayloadLen, "small random datagrams, as the ordinary punch sends")
		}
	}()

	start := time.Now()
	err = a.BirthdaySpray(ctx, "127.0.0.1", sinkPort, sinkPort, 4000, types.PeerID{7})
	assert.ErrorIs(t, err, ErrHolePunchTimeout, "nobody arrived, so it runs out the window")
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 2*time.Second, "paced over the window, not a single burst")
	assert.LessOrEqual(t, elapsed, 4*time.Second, "bounded by the punch window")
}

// A spray stops as soon as the peer's connection shows up.
func TestBirthdaySprayStopsOnArrival(t *testing.T) {
	a, _ := newQuicWithID(t)
	ctx := context.Background()
	require.NoError(t, a.ListenEarly(ctx))
	peer := types.PeerID{9}

	res := make(chan error, 1)
	go func() { res <- a.BirthdaySpray(ctx, "127.0.0.1", 40000, 44000, 1000, peer) }()
	time.Sleep(150 * time.Millisecond)
	a.arrivalNotify(peer) // as newConn does when the peer's connection is authenticated

	select {
	case err := <-res:
		assert.NoError(t, err, "the arrival ends the spray cleanly")
	case <-time.After(2 * time.Second):
		t.Fatal("spray did not stop when the peer arrived")
	}
}

// With nobody to connect to, BirthdayDial opens its sockets, punches, and gives up within the window
// instead of hanging; the socket count is capped.
func TestBirthdayDialTimesOut(t *testing.T) {
	a, _ := newQuicWithID(t)
	ctx := context.Background()
	require.NoError(t, a.ListenEarly(ctx))

	start := time.Now()
	err := a.BirthdayDial(ctx, "127.0.0.1:9", types.PeerID{9}, 10000) // asks for more than the cap
	assert.ErrorIs(t, err, ErrHolePunchTimeout)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// The mutual (both-symmetric) dialer gives up within the punch window when nobody answers.
func TestBirthdayMutualDialTimesOut(t *testing.T) {
	a, _ := newQuicWithID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	require.NoError(t, a.ListenEarly(ctx))
	start := time.Now()
	err := a.BirthdayMutual(ctx, "127.0.0.1", 40000, 44000, 8, types.PeerID{9}, true)
	assert.ErrorIs(t, err, ErrHolePunchTimeout)
	assert.Less(t, time.Since(start), 3*time.Second)
}

// The mutual listener stands up its per-socket listeners and gives up when the dialer never comes.
func TestBirthdayMutualListenTimesOut(t *testing.T) {
	a, _ := newQuicWithID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	require.NoError(t, a.ListenEarly(ctx))
	start := time.Now()
	err := a.BirthdayMutual(ctx, "127.0.0.1", 40000, 44000, 8, types.PeerID{9}, false)
	assert.ErrorIs(t, err, ErrHolePunchTimeout)
	assert.Less(t, time.Since(start), 3*time.Second)
}

// Two mutual punches toward the same peer and IP do not run at once.
func TestBirthdayMutualIsDeduped(t *testing.T) {
	a, _ := newQuicWithID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, a.ListenEarly(ctx))
	first := make(chan error, 1)
	go func() { first <- a.BirthdayMutual(ctx, "127.0.0.1", 40000, 44000, 8, types.PeerID{9}, true) }()
	time.Sleep(150 * time.Millisecond)
	assert.ErrorIs(t, a.BirthdayMutual(ctx, "127.0.0.1", 40000, 44000, 8, types.PeerID{9}, false), ErrPunchActive)
	<-first
}

// Two birthday dials toward the same peer and address do not run at once.
func TestBirthdayDialIsDedupedPerPeerAndAddr(t *testing.T) {
	a, _ := newQuicWithID(t)
	ctx := context.Background()
	require.NoError(t, a.ListenEarly(ctx))

	first := make(chan error, 1)
	go func() { first <- a.BirthdayDial(ctx, "127.0.0.1:9", types.PeerID{9}, 8) }()
	time.Sleep(150 * time.Millisecond)
	assert.ErrorIs(t, a.BirthdayDial(ctx, "127.0.0.1:9", types.PeerID{9}, 8), ErrPunchActive)
	<-first
}
