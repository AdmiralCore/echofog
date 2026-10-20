package exit_gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memLink is one direction pair of an in-memory datagram path that takes frames up to max bytes.
type memLink struct {
	max   int
	drop  func(frame []byte) bool // nil: deliver everything
	sent  atomic.Int64
	peer  *memLink
	mu    sync.Mutex
	subs  map[uint32]func([]byte)
	limit atomic.Int64 // frames sent past this many are refused (0: no limit)
}

func newMemLinks(max int) (client, exit *memLink) {
	client = &memLink{max: max, subs: map[uint32]func([]byte){}}
	exit = &memLink{max: max, subs: map[uint32]func([]byte){}}
	client.peer, exit.peer = exit, client
	return client, exit
}

func (l *memLink) send(frame []byte) error {
	if len(frame) > l.max {
		return &network.DatagramTooLargeError{Size: len(frame), Max: l.max}
	}
	l.sent.Add(1)
	if l.drop != nil && l.drop(frame) {
		return nil
	}
	p := l.peer
	p.mu.Lock()
	fn := p.subs[uint32(frame[0])<<24|uint32(frame[1])<<16|uint32(frame[2])<<8|uint32(frame[3])]
	p.mu.Unlock()
	if fn != nil {
		fn(append([]byte(nil), frame[4:]...))
	}
	return nil
}

func (l *memLink) subscribe(id uint32, fn func([]byte)) func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.subs[id] != nil {
		return nil
	}
	l.subs[id] = fn
	return func() { l.mu.Lock(); delete(l.subs, id); l.mu.Unlock() }
}

// dgPipe is a pipe stream that offers a datagram link, like a swarm stream over QUIC.
type dgPipe struct {
	*pipeStream
	link udpLink
}

func (d dgPipe) udpLink(bool) udpLink { return d.link }

// serveUDPDatagrams runs a new exit (datagrams on) behind an in-memory link of the given size.
func serveUDPDatagrams(svc *ExitService, max int) (StreamOpener, *memLink, *memLink) {
	cl, ex := newMemLinks(max)
	return func(ctx context.Context) (FramedStream, error) {
		client, server := newPipePair()
		go func() {
			if _, hello, err := server.ReadFrame(); err == nil {
				svc.handleUDP(context.Background(), dgPipe{server, ex}, types.PeerID{}, hello)
			}
		}()
		return dgPipe{client, cl}, nil
	}, cl, ex
}

// bigUDPEcho echoes datagrams of any size.
func bigUDPEcho(t *testing.T) *net.UDPConn {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, maxDatagram)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = c.WriteToUDP(buf[:n], from)
		}
	}()
	return c
}

func openExit() *ExitService {
	svc := NewExitService()
	svc.allowPrivate.Store(true)
	svc.SetEnabled(true)
	return svc
}

// Datagrams travel outside the stream, cut into fragments when the path is smaller than they
// are, and on the stream when they would need too many fragments.
func TestUDPDatagramsFragmentAndFallBack(t *testing.T) {
	echo := bigUDPEcho(t).LocalAddr().String()
	open, cl, ex := serveUDPDatagrams(openExit(), 600)
	u, err := NewP2PDialer(open).DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u.Close()
	require.True(t, u.Datagrams(), "both ends can: the session uses datagrams")

	for _, size := range []int{1, 500, 1400, 4000, 30000} {
		msg := make([]byte, size)
		_, _ = rand.Read(msg)
		before := cl.sent.Load()
		require.NoError(t, u.WriteTo(msg, echo))
		got, from := readWithin(t, u, 3*time.Second)
		assert.Equal(t, echo, from)
		require.True(t, bytes.Equal(msg, got), "size %d came back intact", size)
		frags := cl.sent.Load() - before
		switch {
		case size <= 500:
			assert.Equal(t, int64(1), frags, "fits: one datagram")
		case size <= 4000:
			assert.Greater(t, frags, int64(1), "cut into fragments")
		default:
			assert.Zero(t, frags, "too many fragments: sent on the stream")
		}
	}
	assert.Positive(t, ex.sent.Load(), "replies came back as datagrams too")
}

// A fragment lost loses its datagram, like UDP itself, and does not wedge the ones after it.
func TestUDPDatagramLostFragmentDoesNotWedge(t *testing.T) {
	echo := bigUDPEcho(t).LocalAddr().String()
	open, cl, _ := serveUDPDatagrams(openExit(), 600)
	var dropOnce atomic.Bool
	cl.drop = func(frame []byte) bool {
		// the second fragment of the first fragmented datagram goes missing
		return frame[7] > 1 && frame[6] == 1 && dropOnce.CompareAndSwap(false, true)
	}
	u, err := NewP2PDialer(open).DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u.Close()

	require.NoError(t, u.WriteTo(bytes.Repeat([]byte{1}, 1500), echo)) // loses a piece
	require.NoError(t, u.WriteTo(bytes.Repeat([]byte{2}, 1500), echo))
	got, _ := readWithin(t, u, 3*time.Second)
	assert.Equal(t, bytes.Repeat([]byte{2}, 1500), got, "the next datagram arrives whole")
}

// A new client and an exit that predates datagrams agree on the stream.
func TestUDPDatagramsFallBackToAnOldExit(t *testing.T) {
	echo := bigUDPEcho(t).LocalAddr().String()
	svc := openExit()
	cl, _ := newMemLinks(1200)
	open := func(ctx context.Context) (FramedStream, error) {
		client, server := newPipePair()
		go func() {
			if _, _, err := server.ReadFrame(); err == nil {
				svc.HandleUDPFrom(context.Background(), server, types.PeerID{}) // ignores the hello
			}
		}()
		return dgPipe{client, cl}, nil
	}
	u, err := NewP2PDialer(open).DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u.Close()
	assert.False(t, u.Datagrams())
	require.NoError(t, u.WriteTo([]byte("old exit"), echo))
	got, _ := readWithin(t, u, 3*time.Second)
	assert.Equal(t, "old exit", string(got))
	assert.Zero(t, cl.sent.Load())
}

// One target whose name takes long to resolve does not hold up datagrams to the others.
func TestUDPSlowLookupDoesNotStallTheAssociation(t *testing.T) {
	echo := bigUDPEcho(t).LocalAddr().String()
	svc := openExit()
	release := make(chan struct{})
	defer close(release)
	svc.resolver = func(ctx context.Context, host string) ([]net.IP, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, &net.DNSError{Err: "timeout", Name: host}
	}
	u, err := NewP2PDialer(serveUDP(svc, types.PeerID{})).DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u.Close()

	require.NoError(t, u.WriteTo([]byte("to a slow name"), "slow.test:53"))
	start := time.Now()
	require.NoError(t, u.WriteTo([]byte("to an address"), echo))
	got, _ := readWithin(t, u, 3*time.Second)
	assert.Equal(t, "to an address", string(got))
	assert.Less(t, time.Since(start), time.Second, "the reply did not wait for the slow lookup")
}

func TestUDPFragmentCodecAndDefrag(t *testing.T) {
	f, ok := decodeUDPFragment(encodeUDPFragment(7, 9, 1, 3, "a.b:1", []byte("xy"))[4:])
	require.True(t, ok)
	assert.Equal(t, udpFragment{pkt: 9, idx: 1, count: 3, addr: "a.b:1", data: []byte("xy")}, f)

	for _, bad := range [][]byte{
		{},
		encodeUDPFragment(1, 1, 3, 3, "a:1", nil)[4:],  // index past count
		encodeUDPFragment(1, 1, 0, 0, "a:1", nil)[4:],  // no fragments
		encodeUDPFragment(1, 1, 0, 99, "a:1", nil)[4:], // far too many
		encodeUDPFragment(1, 1, 0, 1, "", nil)[4:],     // no address
	} {
		_, ok := decodeUDPFragment(bad)
		assert.False(t, ok)
	}

	var d defrag
	part := func(pkt uint16, idx uint8, s string) ([]byte, bool) {
		return d.feed(udpFragment{pkt: pkt, idx: idx, count: 3, addr: "a:1", data: []byte(s)})
	}
	_, done := part(1, 2, "c")
	assert.False(t, done)
	_, done = part(1, 0, "a")
	assert.False(t, done)
	_, done = part(1, 0, "a")
	assert.False(t, done, "a repeated fragment is ignored")
	got, done := part(1, 1, "b")
	require.True(t, done)
	assert.Equal(t, "abc", string(got), "out of order fragments are put back in order")

	part(2, 0, "x")
	_, done = part(3, 1, "y") // packet 2 lost a piece; 3 starts over
	assert.False(t, done)
	part(3, 0, "x")
	got, done = part(3, 2, "z")
	require.True(t, done)
	assert.Equal(t, "xyz", string(got))
}

// Over real QUIC the session switches to datagrams; over the TCP fallback (see
// TestExitAndMediaOverTCPFallback) it stays on the stream.
func TestUDPDatagramsThroughRealSwarm(t *testing.T) {
	exitSwarm, exitTr := newSwarm(t)
	clientSwarm, clientTr := newSwarm(t)
	AttachToSwarm(exitSwarm, openExit())
	require.NoError(t, clientTr.DialEarly(context.Background(), exitTr.Addr().String()))
	require.Eventually(t, func() bool {
		return len(exitSwarm.GetAllPeers()) > 0 && len(clientSwarm.GetAllPeers()) > 0
	}, 5*time.Second, 50*time.Millisecond)
	d := NewP2PDialer(SwarmOpener(clientSwarm, clientSwarm.GetAllPeers()[0].ID()))

	echo := bigUDPEcho(t).LocalAddr().String()
	u, err := d.DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u.Close()
	require.True(t, u.Datagrams())
	for _, size := range []int{100, 1400, 3000, 20000} {
		msg := make([]byte, size)
		_, _ = rand.Read(msg)
		require.NoError(t, u.WriteTo(msg, echo))
		got, _ := readWithin(t, u, 3*time.Second)
		require.True(t, bytes.Equal(msg, got), "size %d", size)
	}

	// two sessions at once keep their datagrams apart
	u2, err := d.DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u2.Close()
	require.NoError(t, u2.WriteTo([]byte("second"), echo))
	got, _ := readWithin(t, u2, 3*time.Second)
	assert.Equal(t, "second", string(got))
	require.NoError(t, u.WriteTo([]byte("first"), echo))
	got, _ = readWithin(t, u, 3*time.Second)
	assert.Equal(t, "first", string(got))
}
