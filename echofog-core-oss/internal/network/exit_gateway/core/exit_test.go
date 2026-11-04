package exit_gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// simulatedP2PPipe simulates bidirectional stream transfer across P2P virtual circuits
type simulatedP2PPipe struct {
	clientReader *io.PipeReader
	clientWriter *io.PipeWriter
	serverReader *io.PipeReader
	serverWriter *io.PipeWriter
}

func newSimulatedP2PPipe() (*simulatedP2PPipe, *p2pSide, *p2pSide) {
	cr, cw := io.Pipe()
	sr, sw := io.Pipe()

	clientSide := &p2pSide{r: sr, w: cw}
	serverSide := &p2pSide{r: cr, w: sw}

	pipe := &simulatedP2PPipe{
		clientReader: cr, clientWriter: cw,
		serverReader: sr, serverWriter: sw,
	}

	return pipe, clientSide, serverSide
}

type p2pSide struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (s *p2pSide) Read(p []byte) (n int, err error)  { return s.r.Read(p) }
func (s *p2pSide) Write(p []byte) (n int, err error) { return s.w.Write(p) }
func (s *p2pSide) Close() error {
	_ = s.r.Close()
	return s.w.Close()
}

type testExitDialer struct {
	splicer *ExitSplicer
}

func (d *testExitDialer) DialExitStream(ctx context.Context, targetAddr string) (io.ReadWriteCloser, error) {
	_, clientSide, serverSide := newSimulatedP2PPipe()

	// Exit node starts splicing asynchronously
	go func() {
		defer serverSide.Close()
		_ = d.splicer.HandleExitStream(ctx, serverSide, targetAddr)
	}()

	return clientSide, nil
}

func TestSOCKS5AndExitSplicing_EndToEnd(t *testing.T) {
	// 1. Start Mock Target Web Server (simulates Wikipedia or YouTube in free internet)
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer targetListener.Close()

	targetAddr := targetListener.Addr().String()

	go func() {
		for {
			conn, err := targetListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				// Echo back with prefix
				resp := append([]byte("ECHO: "), buf[:n]...)
				_, _ = c.Write(resp)
			}(conn)
		}
	}()

	// 2. Start ExitSplicer
	splicer := NewExitSplicer(3 * time.Second)

	// 3. Start Local SOCKS5 Proxy Server
	dialer := &testExitDialer{splicer: splicer}
	socksServer := NewSOCKS5Server("127.0.0.1:0", dialer)
	err = socksServer.Start()
	require.NoError(t, err)
	defer func() { _ = socksServer.Stop() }()

	socksAddr := socksServer.Addr().String()
	require.NotEmpty(t, socksAddr)

	// 4. Test SOCKS5 Client Connection (IPv4 target)
	clientConn, err := net.Dial("tcp", socksAddr)
	require.NoError(t, err)
	defer clientConn.Close()

	// SOCKS5 Auth Handshake: [VER 0x05, 1 Method, Method 0x00 (NO AUTH)]
	_, err = clientConn.Write([]byte{0x05, 0x01, 0x00})
	require.NoError(t, err)

	authResp := make([]byte, 2)
	_, err = io.ReadFull(clientConn, authResp)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x05, 0x00}, authResp, "SOCKS5 should accept NO AUTH")

	// SOCKS5 Connect Request: [VER 0x05, CMD 0x01 (CONNECT), RSV 0x00, ATYP 0x01 (IPv4), IP, Port]
	host, portStr, _ := net.SplitHostPort(targetAddr)
	var port uint16
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	ipBytes := net.ParseIP(host).To4()
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, ipBytes...)
	req = append(req, byte(port>>8), byte(port&0xFF))

	_, err = clientConn.Write(req)
	require.NoError(t, err)

	// SOCKS5 Connect Reply
	connectResp := make([]byte, 10)
	_, err = io.ReadFull(clientConn, connectResp)
	require.NoError(t, err)
	assert.Equal(t, byte(0x05), connectResp[0])
	assert.Equal(t, byte(0x00), connectResp[1], "Reply code 0x00 means Success")

	// Send application payload through the tunnel
	testPayload := []byte("HELLO_FROM_SOCKS5")
	_, err = clientConn.Write(testPayload)
	require.NoError(t, err)

	echoBuf := make([]byte, 1024)
	n, err := clientConn.Read(echoBuf)
	require.NoError(t, err)
	assert.Equal(t, "ECHO: HELLO_FROM_SOCKS5", string(echoBuf[:n]))

	// Check telemetry
	time.Sleep(50 * time.Millisecond)
	active, rx, tx := socksServer.Stats()
	assert.GreaterOrEqual(t, active, int64(0))
	assert.Greater(t, rx, uint64(0))
	assert.Greater(t, tx, uint64(0))

	servedStreams, totalBytes := splicer.Stats()
	assert.GreaterOrEqual(t, servedStreams, uint64(1))
	assert.Greater(t, totalBytes, uint64(0))
}

func TestSOCKS5_DomainNameResolutionAndErrors(t *testing.T) {
	// Mock target
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer targetListener.Close()

	_, portStr, _ := net.SplitHostPort(targetListener.Addr().String())
	var port uint16
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	splicer := NewExitSplicer(2 * time.Second)
	dialer := &testExitDialer{splicer: splicer}
	socksServer := NewSOCKS5Server("127.0.0.1:0", dialer)
	require.NoError(t, socksServer.Start())
	defer func() { _ = socksServer.Stop() }()

	// 1. Connect using Domain Name ATYP (0x03)
	conn, err := net.Dial("tcp", socksServer.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	_, _ = conn.Write([]byte{0x05, 0x01, 0x00})
	authResp := make([]byte, 2)
	_, _ = io.ReadFull(conn, authResp)

	// ATYP 0x03: Domain name "127.0.0.1" (or "localhost")
	domain := "localhost"
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(domain))}
	req = append(req, []byte(domain)...)
	req = append(req, byte(port>>8), byte(port&0xFF))

	_, err = conn.Write(req)
	require.NoError(t, err)

	resp := make([]byte, 10)
	_, err = io.ReadFull(conn, resp)
	require.NoError(t, err)
	assert.Equal(t, byte(0x00), resp[1]) // Success

	// 2. Unsupported Command (e.g., BIND 0x02 instead of CONNECT 0x01)
	connBad, err := net.Dial("tcp", socksServer.Addr().String())
	require.NoError(t, err)
	defer connBad.Close()

	_, _ = connBad.Write([]byte{0x05, 0x01, 0x00})
	_, _ = io.ReadFull(connBad, authResp)

	// CMD 0x02 (BIND)
	badReq := []byte{0x05, 0x02, 0x00, 0x01, 127, 0, 0, 1, 0, 80}
	_, _ = connBad.Write(badReq)

	badResp := make([]byte, 10)
	_, err = io.ReadFull(connBad, badResp)
	require.NoError(t, err)
	assert.Equal(t, byte(0x07), badResp[1], "Should return Command Not Supported (0x07)")

	// 3. Invalid SOCKS version (e.g., SOCKS4)
	connV4, err := net.Dial("tcp", socksServer.Addr().String())
	require.NoError(t, err)
	defer connV4.Close()
	_, _ = connV4.Write([]byte{0x04, 0x01, 0x00}) // Ver 4
	// Connection should be closed by server

	// 4. Unsupported address type (e.g., 0xFF)
	connAtyp, err := net.Dial("tcp", socksServer.Addr().String())
	require.NoError(t, err)
	defer connAtyp.Close()
	_, _ = connAtyp.Write([]byte{0x05, 0x01, 0x00})
	_, _ = io.ReadFull(connAtyp, authResp)
	_, _ = connAtyp.Write([]byte{0x05, 0x01, 0x00, 0xFF}) // Bad ATYP
	_, _ = io.ReadFull(connAtyp, badResp)
	assert.Equal(t, byte(0x08), badResp[1]) // Address type not supported

	// 5. Server with nil dialer
	nilServer := NewSOCKS5Server("127.0.0.1:0", nil)
	require.NoError(t, nilServer.Start())
	defer func() { _ = nilServer.Stop() }()

	connNil, err := net.Dial("tcp", nilServer.Addr().String())
	require.NoError(t, err)
	defer connNil.Close()
	_, _ = connNil.Write([]byte{0x05, 0x01, 0x00})
	_, _ = io.ReadFull(connNil, authResp)
	_, _ = connNil.Write([]byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0, 80})
	_, _ = io.ReadFull(connNil, badResp)
	assert.Equal(t, byte(0x01), badResp[1]) // General failure

	// 6. IPv6 target test
	connIPv6, err := net.Dial("tcp", socksServer.Addr().String())
	require.NoError(t, err)
	defer connIPv6.Close()
	_, _ = connIPv6.Write([]byte{0x05, 0x01, 0x00})
	_, _ = io.ReadFull(connIPv6, authResp)

	ipv6Req := []byte{0x05, 0x01, 0x00, 0x04}
	ipv6Req = append(ipv6Req, net.ParseIP("::1")...)
	ipv6Req = append(ipv6Req, byte(port>>8), byte(port&0xFF))
	_, _ = connIPv6.Write(ipv6Req)
	_, _ = io.ReadFull(connIPv6, resp)
	assert.Equal(t, byte(0x00), resp[1]) // Success
}

// A blackholed first address must not use up the dial budget before a working one is tried.
func TestRaceDialSkipsADeadAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	// 192.0.2.1 (TEST-NET-1) never answers; 127.0.0.1 does
	c := raceDial(ctx, []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("127.0.0.1")}, port)
	require.NotNil(t, c)
	c.Close()
	assert.Less(t, time.Since(start), time.Second, "the second address is tried after the stagger, not after a timeout")
	assert.Equal(t, []string{"1.1.1.1", "::1", "8.8.8.8"}, func() (out []string) {
		for _, ip := range interleaveFamilies([]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("8.8.8.8"), net.ParseIP("::1")}) {
			out = append(out, ip.String())
		}
		return
	}())
}
