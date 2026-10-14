package exit_gateway

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type frame struct {
	t net_types.MessageType
	b []byte
}

// pipeStream is one end of an in-memory bidirectional stream with a framed handshake
type pipeStream struct {
	net.Conn
	in  chan frame
	out chan frame
}

func newPipePair() (*pipeStream, *pipeStream) {
	a, b := net.Pipe()
	ab, ba := make(chan frame, 4), make(chan frame, 4)
	return &pipeStream{Conn: a, in: ba, out: ab}, &pipeStream{Conn: b, in: ab, out: ba}
}

func (p *pipeStream) WriteFrame(t net_types.MessageType, d []byte) error {
	p.out <- frame{t, append([]byte(nil), d...)}
	return nil
}

func (p *pipeStream) ReadFrame() (net_types.MessageType, []byte, error) {
	f, ok := <-p.in
	if !ok {
		return 0, nil, io.EOF
	}
	return f.t, f.b, nil
}

func echoServer(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

// serve runs the exit side for each stream the dialer opens
func serve(svc *ExitService) StreamOpener {
	return func(ctx context.Context) (FramedStream, error) {
		client, server := newPipePair()
		go func() {
			_, target, err := server.ReadFrame()
			if err != nil {
				return
			}
			svc.HandleTunnel(context.Background(), server, target)
		}()
		return client, nil
	}
}

func TestTunnelRoundTripThroughExit(t *testing.T) {
	target := echoServer(t)
	svc := NewExitService()
	svc.allowPrivate = true
	svc.SetEnabled(true)

	conn, err := NewP2PDialer(serve(svc)).DialExitStream(context.Background(), target)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("hello exit"))
	require.NoError(t, err)

	buf := make([]byte, 10)
	conn.(net.Conn).SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "hello exit", string(buf))

	acc, ref, _ := svc.Stats()
	assert.Equal(t, uint64(1), acc)
	assert.Equal(t, uint64(0), ref)
}

func TestExitRefusesWhenDisabled(t *testing.T) {
	svc := NewExitService()
	svc.allowPrivate = true
	_, err := NewP2PDialer(serve(svc)).DialExitStream(context.Background(), echoServer(t))
	assert.ErrorIs(t, err, ErrExitDenied)
}

func TestExitBlocksPrivateTargets(t *testing.T) {
	svc := NewExitService()
	svc.SetEnabled(true)
	for _, target := range []string{"127.0.0.1:80", "192.168.1.1:80", "10.0.0.5:22", "169.254.169.254:80", "100.64.0.1:80", "[::1]:80"} {
		_, err := NewP2PDialer(serve(svc)).DialExitStream(context.Background(), target)
		assert.ErrorIs(t, err, ErrExitDenied, target)
	}
	_, err := NewP2PDialer(serve(svc)).DialExitStream(context.Background(), "no-port")
	assert.ErrorIs(t, err, ErrExitDenied)
}

func TestExitBlocksHostnamesResolvingToPrivate(t *testing.T) {
	svc := NewExitService()
	svc.SetEnabled(true)
	svc.resolver = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.1.2.3")}, nil }
	_, err := NewP2PDialer(serve(svc)).DialExitStream(context.Background(), "evil.example:443")
	assert.ErrorIs(t, err, ErrExitDenied)
}

func TestDialerValidatesInput(t *testing.T) {
	_, err := NewP2PDialer(nil).DialExitStream(context.Background(), "a:1")
	assert.Error(t, err)
	_, err = NewP2PDialer(serve(NewExitService())).DialExitStream(context.Background(), "")
	assert.ErrorIs(t, err, ErrInvalidTarget)
}

func TestFreeQuotaThenPaymentRequired(t *testing.T) {
	target := echoServer(t)
	svc := NewExitService()
	svc.allowPrivate = true
	svc.SetEnabled(true)
	svc.SetPolicy(Policy{PricePerMB: 5, FreeBytes: 1024})

	var peer types.PeerID
	peer[0] = 7
	open := func(ctx context.Context) (FramedStream, error) {
		client, server := newPipePair()
		go func() {
			_, tgt, err := server.ReadFrame()
			if err == nil {
				svc.HandleTunnelFrom(context.Background(), server, tgt, peer)
			}
		}()
		return client, nil
	}

	conn, err := NewP2PDialer(open).DialExitStream(context.Background(), target)
	require.NoError(t, err)
	payload := make([]byte, 600)
	_, err = conn.Write(payload)
	require.NoError(t, err)
	_, err = io.ReadFull(conn, make([]byte, 600))
	require.NoError(t, err)
	conn.Close()

	require.Eventually(t, func() bool { return svc.allowance(peer) == 0 }, 2*time.Second, 20*time.Millisecond)

	_, err = NewP2PDialer(open).DialExitStream(context.Background(), target)
	assert.ErrorIs(t, err, ErrPaymentRequired)

	var other types.PeerID
	other[0] = 9
	assert.Equal(t, int64(1024), svc.allowance(other))

	svc.SetPolicy(Policy{})
	assert.Equal(t, int64(-1), svc.allowance(peer), "a free exit has no quota")
}
