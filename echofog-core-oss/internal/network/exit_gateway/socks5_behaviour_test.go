package exit_gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/fakedns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// unreachableDialer stands for an exit that is down: every dial fails, and counts.
type unreachableDialer struct{ tcp, udp atomic.Int32 }

func (d *unreachableDialer) DialExitStream(context.Context, string) (io.ReadWriteCloser, error) {
	d.tcp.Add(1)
	return nil, errors.New("exit is down")
}

func (d *unreachableDialer) DialExitUDP(context.Context) (*UDPSession, error) {
	d.udp.Add(1)
	return nil, errors.New("exit is down")
}

func quietServer(t *testing.T, d StreamDialer) *SOCKS5Server {
	srv := NewSOCKS5Server("127.0.0.1:0", d)
	srv.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, srv.Start())
	t.Cleanup(func() { srv.Stop() })
	return srv
}

func answerIP(t *testing.T, resp []byte) net.IP {
	var p dnsmessage.Parser
	_, err := p.Start(resp)
	require.NoError(t, err)
	require.NoError(t, p.SkipAllQuestions())
	a, err := p.AnswerHeader()
	require.NoError(t, err)
	require.Equal(t, dnsmessage.TypeA, a.Type)
	r, err := p.AResource()
	require.NoError(t, err)
	return net.IP(r.A[:])
}

// In whole-computer mode name resolution is answered locally and must keep working when the
// exit is down or not paid for; it must not even try the exit for that.
func TestFakeDNSWorksWithoutTheExit(t *testing.T) {
	d := &unreachableDialer{}
	srv := quietServer(t, d)
	m := fakedns.New()
	srv.SetFakeDNS(m)

	_, relay := socksAssociate(t, srv.Addr().String())
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer pc.Close()
	_, err = pc.WriteToUDP(socksUDPPacket(fakedns.DNSAddr, 53, dnsQuery(t, "example.com.")), relay)
	require.NoError(t, err)
	_, _, resp := readSocksUDP(t, pc)
	assert.Equal(t, m.IPFor("example.com"), answerIP(t, resp), "UDP: answered from the fake DNS")

	// the same over TCP, as resolvers do after a truncated answer
	c, err := socksDial(srv.Addr().String(), net.JoinHostPort(fakedns.DNSAddr.String(), "53"))
	require.NoError(t, err)
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	require.NoError(t, writeDNSTCP(c, dnsQuery(t, "example.org.")))
	resp, err = readDNSTCP(c)
	require.NoError(t, err)
	assert.Equal(t, m.IPFor("example.org"), answerIP(t, resp), "TCP: answered from the fake DNS")

	assert.Zero(t, d.udp.Load(), "no exit session for a locally answered query")
	assert.Zero(t, d.tcp.Load())
}

// When the association does need the exit and cannot get it, the client is told by the control
// connection closing, instead of sending into a port nobody reads.
func TestUDPAssociateEndsWhenTheExitCannotBeReached(t *testing.T) {
	d := &unreachableDialer{}
	srv := quietServer(t, d)
	ctrl, relay := socksAssociate(t, srv.Addr().String())

	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer pc.Close()
	_, err = pc.WriteToUDP(socksUDPPacket(net.IPv4(203, 0, 113, 1), 443, []byte("quic")), relay)
	require.NoError(t, err)

	ctrl.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = ctrl.Read(make([]byte, 1))
	require.Error(t, err)
	assert.False(t, errors.Is(err, context.DeadlineExceeded) || isTimeout(err), "the control connection must close, not hang")
	assert.Equal(t, int32(1), d.udp.Load())
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// RFC 1928: a client that offers no method we accept gets 0xFF, not a silent "no auth".
func TestSOCKSRejectsClientsWithoutNoAuth(t *testing.T) {
	srv := quietServer(t, okDialer{})
	c, err := net.Dial("tcp", srv.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Write([]byte{5, 1, 2}) // username/password only
	require.NoError(t, err)
	rep := make([]byte, 2)
	_, err = io.ReadFull(c, rep)
	require.NoError(t, err)
	assert.Equal(t, []byte{5, 0xFF}, rep)
}

// Turning the VPN off ends the connections that were open, so nothing keeps using the exit.
func TestSOCKSStopEndsOpenConnections(t *testing.T) {
	srv := quietServer(t, okDialer{})
	c, err := socksDial(srv.Addr().String(), "203.0.113.5:80")
	require.NoError(t, err)
	defer c.Close()
	require.Eventually(t, func() bool { a, _, _ := srv.Stats(); return a == 1 }, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, srv.Stop())
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Read(make([]byte, 1))
	require.Error(t, err)
	assert.False(t, isTimeout(err), "stopped proxy must close the connection")
	require.Eventually(t, func() bool { a, _, _ := srv.Stats(); return a == 0 }, 2*time.Second, 10*time.Millisecond)
}

// Byte counters move while a long transfer runs, not only when it ends.
func TestSOCKSCountsBytesLive(t *testing.T) {
	srv := quietServer(t, okDialer{})
	c, err := socksDial(srv.Addr().String(), "203.0.113.5:80")
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write(make([]byte, 100_000))
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, _, tx := srv.Stats(); return tx >= 100_000 }, 2*time.Second, 10*time.Millisecond)
}
