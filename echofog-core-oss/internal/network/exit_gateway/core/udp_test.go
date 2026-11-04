package exit_gateway

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func udpEchoServer(t *testing.T) *net.UDPConn {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
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

func serveUDP(svc *ExitService, peer types.PeerID) StreamOpener {
	return func(ctx context.Context) (FramedStream, error) {
		client, server := newPipePair()
		go func() {
			if _, _, err := server.ReadFrame(); err == nil {
				svc.HandleUDPFrom(context.Background(), server, peer)
			}
		}()
		return client, nil
	}
}

func readWithin(t *testing.T, u *UDPSession, d time.Duration) ([]byte, string) {
	type res struct {
		b    []byte
		addr string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		b, a, err := u.ReadFrom()
		ch <- res{b, a, err}
	}()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		return r.b, r.addr
	case <-time.After(d):
		t.Fatal("no datagram")
		return nil, ""
	}
}

func TestDatagramCodec(t *testing.T) {
	f, err := encodeDatagram("example.com:53", []byte{1, 2, 3})
	require.NoError(t, err)
	addr, p, err := decodeDatagram(f)
	require.NoError(t, err)
	assert.Equal(t, "example.com:53", addr)
	assert.Equal(t, []byte{1, 2, 3}, p)

	_, _, err = decodeDatagram([]byte{0})
	assert.ErrorIs(t, err, ErrBadDatagram)
	_, _, err = decodeDatagram([]byte{0, 50, 'a'})
	assert.ErrorIs(t, err, ErrBadDatagram)
	_, err = encodeDatagram("", nil)
	assert.ErrorIs(t, err, ErrBadDatagram)
}

func TestUDPRelayRoundTripLabelsRepliesWithRequestedName(t *testing.T) {
	echo := udpEchoServer(t)
	port := echo.LocalAddr().(*net.UDPAddr).Port
	svc := NewExitService()
	svc.allowPrivate.Store(true)
	svc.SetEnabled(true)
	svc.resolver = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "echo.test" {
			return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}

	u, err := NewP2PDialer(serveUDP(svc, types.PeerID{})).DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u.Close()

	name := net.JoinHostPort("echo.test", itoa(port))
	require.NoError(t, u.WriteTo([]byte("ping by name"), name))
	b, from := readWithin(t, u, 2*time.Second)
	assert.Equal(t, "ping by name", string(b))
	assert.Equal(t, name, from)

	ip := echo.LocalAddr().String()
	require.NoError(t, u.WriteTo([]byte("ping by ip"), ip))
	b, from = readWithin(t, u, 2*time.Second)
	assert.Equal(t, "ping by ip", string(b))
	assert.Contains(t, []string{ip, name}, from)
}

func TestUDPRelayRefusedWhenDisabled(t *testing.T) {
	svc := NewExitService()
	_, err := NewP2PDialer(serveUDP(svc, types.PeerID{})).DialExitUDP(context.Background())
	assert.ErrorIs(t, err, ErrExitDenied)
}

func TestUDPRelayDropsPrivateTargets(t *testing.T) {
	echo := udpEchoServer(t)
	svc := NewExitService()
	svc.SetEnabled(true) // allowPrivate stays false

	u, err := NewP2PDialer(serveUDP(svc, types.PeerID{})).DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u.Close()
	require.NoError(t, u.WriteTo([]byte("x"), echo.LocalAddr().String()))

	got := make(chan struct{})
	go func() { u.ReadFrom(); close(got) }()
	select {
	case <-got:
		t.Fatal("datagram to a loopback target must not be relayed")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestUDPRelayPaymentRequiredWithoutQuota(t *testing.T) {
	svc := NewExitService()
	svc.allowPrivate.Store(true)
	svc.SetEnabled(true)
	svc.SetPolicy(Policy{PricePerMB: 5, FreeBytes: 100})
	var peer types.PeerID
	peer[0] = 9
	require.True(t, svc.consume(peer, 100)) // the free quota is gone
	_, err := NewP2PDialer(serveUDP(svc, peer)).DialExitUDP(context.Background())
	assert.ErrorIs(t, err, ErrPaymentRequired)
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestUDPRelayQuotaNeverWrapsToUnlimited(t *testing.T) {
	svc := NewExitService()
	svc.SetPolicy(Policy{PricePerMB: 5, FreeBytes: 100})
	r := &udpRelay{svc: svc, meter: newMeter(svc, types.PeerID{})}
	assert.True(t, r.account(60))
	assert.False(t, r.account(60)) // crosses the quota
	assert.False(t, r.account(1), "an exhausted relay must stay exhausted")
}

func TestMeteredStreamQuotaNeverWrapsToUnlimited(t *testing.T) {
	svc := NewExitService()
	svc.SetPolicy(Policy{PricePerMB: 5, FreeBytes: 100})
	m := &meteredStream{m: newMeter(svc, types.PeerID{})}
	assert.NoError(t, m.account(60))
	assert.ErrorIs(t, m.account(60), ErrPaymentRequired)
	assert.ErrorIs(t, m.account(1), ErrPaymentRequired)
}
