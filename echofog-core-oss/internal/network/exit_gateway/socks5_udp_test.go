package exit_gateway

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/fakedns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// socksAssociate performs a UDP ASSOCIATE and returns the control connection and relay address.
func socksAssociate(t *testing.T, proxy string) (net.Conn, *net.UDPAddr) {
	c, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(6 * time.Second))
	_, err = c.Write([]byte{5, 1, 0})
	require.NoError(t, err)
	_, err = io.ReadFull(c, make([]byte, 2))
	require.NoError(t, err)
	_, err = c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	require.NoError(t, err)
	resp := make([]byte, 10)
	_, err = io.ReadFull(c, resp)
	require.NoError(t, err)
	require.Equal(t, byte(0), resp[1], "socks reply code")
	c.SetDeadline(time.Time{})
	return c, &net.UDPAddr{IP: net.IP(resp[4:8]), Port: int(binary.BigEndian.Uint16(resp[8:]))}
}

func socksUDPPacket(ip net.IP, port int, data []byte) []byte {
	out := []byte{0, 0, 0, 1}
	out = append(out, ip.To4()...)
	out = append(out, byte(port>>8), byte(port))
	return append(out, data...)
}

func readSocksUDP(t *testing.T, pc *net.UDPConn) (net.IP, int, []byte) {
	pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, _, err := pc.ReadFromUDP(buf)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 10)
	require.Equal(t, byte(1), buf[3])
	return net.IP(append([]byte(nil), buf[4:8]...)), int(binary.BigEndian.Uint16(buf[8:10])), buf[10:n]
}

func dnsQuery(t *testing.T, name string) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	require.NoError(t, b.StartQuestions())
	require.NoError(t, b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}))
	q, err := b.Finish()
	require.NoError(t, err)
	return q
}

// The whole TUN data path minus the TUN device: an application resolves a name through the
// fake DNS, sends UDP and opens TCP to the fake IP, and the exit reaches the real host by name.
func TestSocksUDPAndFakeDNSThroughRealSwarm(t *testing.T) {
	exitSwarm, exitTr := newSwarm(t)
	clientSwarm, clientTr := newSwarm(t)

	svc := NewExitService()
	svc.allowPrivate = true
	svc.SetEnabled(true)
	svc.resolver = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "echo.test" {
			return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	AttachToSwarm(exitSwarm, svc)

	require.NoError(t, clientTr.DialEarly(context.Background(), exitTr.Addr().String()))
	require.Eventually(t, func() bool {
		return len(exitSwarm.GetAllPeers()) > 0 && len(clientSwarm.GetAllPeers()) > 0
	}, 5*time.Second, 50*time.Millisecond)

	exitID := clientSwarm.GetAllPeers()[0].ID()
	srv := NewSOCKS5Server("127.0.0.1:0", NewP2PDialer(SwarmOpener(clientSwarm, exitID)))
	srv.SetFakeDNS(fakedns.New())
	require.NoError(t, srv.Start())
	t.Cleanup(func() { srv.Stop() })

	_, relay := socksAssociate(t, srv.Addr().String())
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer pc.Close()

	// 1. DNS to the TUN's resolver address is answered locally with a fake IP
	_, err = pc.WriteToUDP(socksUDPPacket(fakedns.DNSAddr, 53, dnsQuery(t, "echo.test.")), relay)
	require.NoError(t, err)
	srcIP, srcPort, data := readSocksUDP(t, pc)
	assert.True(t, srcIP.Equal(fakedns.DNSAddr))
	assert.Equal(t, 53, srcPort)
	var msg dnsmessage.Message
	require.NoError(t, msg.Unpack(data))
	require.Len(t, msg.Answers, 1)
	a := msg.Answers[0].Body.(*dnsmessage.AResource)
	fakeIP := net.IP(a.A[:])
	require.True(t, fakedns.IsFake(fakeIP))

	// 2. UDP to fakeIP:port reaches echo.test:port via the exit, the reply comes from fakeIP:port
	echo := udpEchoServer(t)
	echoPort := echo.LocalAddr().(*net.UDPAddr).Port
	_, err = pc.WriteToUDP(socksUDPPacket(fakeIP, echoPort, []byte("udp via fake ip")), relay)
	require.NoError(t, err)
	srcIP, srcPort, data = readSocksUDP(t, pc)
	assert.True(t, srcIP.Equal(fakeIP))
	assert.Equal(t, echoPort, srcPort)
	assert.Equal(t, "udp via fake ip", string(data))

	// 3. TCP CONNECT to the fake IP is sent to the exit by name
	tcpEcho := echoServer(t)
	_, tcpPort, _ := net.SplitHostPort(tcpEcho)
	c := socksConnect(t, srv.Addr().String(), net.JoinHostPort(fakeIP.String(), tcpPort))
	defer c.Close()
	_, err = c.Write([]byte("tcp via fake ip"))
	require.NoError(t, err)
	got := make([]byte, 15)
	_, err = io.ReadFull(c, got)
	require.NoError(t, err)
	assert.Equal(t, "tcp via fake ip", string(got))

	// 4. a fake IP that was never handed out is refused instead of leaking somewhere
	p, _ := strconv.Atoi(tcpPort)
	stale := net.IPv4(198, 19, 250, 250)
	cc, err := net.DialTimeout("tcp", srv.Addr().String(), 3*time.Second)
	require.NoError(t, err)
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(5 * time.Second))
	cc.Write([]byte{5, 1, 0})
	io.ReadFull(cc, make([]byte, 2))
	cc.Write(append(append([]byte{5, 1, 0, 1}, stale.To4()...), byte(p>>8), byte(p)))
	resp := make([]byte, 10)
	_, err = io.ReadFull(cc, resp)
	require.NoError(t, err)
	assert.Equal(t, byte(0x04), resp[1])
}

func TestParseUDPRequestRejectsFragments(t *testing.T) {
	pkt := socksUDPPacket(net.IPv4(1, 2, 3, 4), 53, []byte("x"))
	addr, raw, data, ok := parseUDPRequest(pkt)
	require.True(t, ok)
	assert.Equal(t, "1.2.3.4:53", addr)
	assert.Equal(t, pkt[3:10], raw)
	assert.Equal(t, []byte("x"), data)

	pkt[2] = 1
	_, _, _, ok = parseUDPRequest(pkt)
	assert.False(t, ok)
	_, _, _, ok = parseUDPRequest([]byte{0, 0, 0, 9})
	assert.False(t, ok)
}
