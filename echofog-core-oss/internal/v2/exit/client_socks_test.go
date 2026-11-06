package exit

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
)

// socksRig is the local SOCKS5 entrance of the v1 core with the exit/2 client as its dialer.
func socksRig(t *testing.T, rc rigConfig) string {
	t.Helper()
	cr := newClientRig(t, rc)
	s := core.NewSOCKS5Server("127.0.0.1:0", cr.cl)
	if err := s.Start(); err != nil {
		t.Fatalf("SOCKS5 Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s.Addr().String()
}

func socksHello(t *testing.T, c net.Conn) {
	t.Helper()
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var rep [2]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil || rep != [2]byte{5, 0} {
		t.Fatalf("method reply %v %v", rep, err)
	}
}

func socksAddr(t *testing.T, hostport string) []byte {
	t.Helper()
	host, p, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(p)
	ip := net.ParseIP(host).To4()
	if ip == nil {
		t.Fatalf("not an IPv4 literal: %q", host)
	}
	out := append([]byte{1}, ip...)
	return binary.BigEndian.AppendUint16(out, uint16(port))
}

func socksReply(t *testing.T, c net.Conn) (byte, net.Addr) {
	t.Helper()
	var h [4]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		t.Fatalf("SOCKS reply: %v", err)
	}
	var skip int
	switch h[3] {
	case 1:
		skip = 4 + 2
	case 4:
		skip = 16 + 2
	default:
		t.Fatalf("reply atyp %d", h[3])
	}
	rest := make([]byte, skip)
	if _, err := io.ReadFull(c, rest); err != nil {
		t.Fatal(err)
	}
	var bound net.Addr
	if h[3] == 1 {
		bound = &net.UDPAddr{IP: net.IP(rest[:4]), Port: int(binary.BigEndian.Uint16(rest[4:]))}
	}
	return h[1], bound
}

// SOCKS5 CONNECT → exit/2 → a local HTTP server.
func TestSOCKS5ThroughExitHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello through exit/2")
	}))
	defer srv.Close()
	socks := socksRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}})

	c, err := net.DialTimeout("tcp", socks, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	socksHello(t, c)
	req := append([]byte{5, 1, 0}, socksAddr(t, srv.Listener.Addr().String())...)
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	if rep, _ := socksReply(t, c); rep != 0 {
		t.Fatalf("CONNECT reply %d", rep)
	}
	if _, err := io.WriteString(c, "GET / HTTP/1.0\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(c)
	if !bytes.Contains(body, []byte("hello through exit/2")) {
		t.Fatalf("response %q", body)
	}
}

// SOCKS5 UDP ASSOCIATE → exit/2 → a local UDP server.
func TestSOCKS5ThroughExitUDP(t *testing.T) {
	target := udpEcho(t, nil)
	socks := socksRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}})

	ctrl, err := net.DialTimeout("tcp", socks, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	_ = ctrl.SetDeadline(time.Now().Add(10 * time.Second))
	socksHello(t, ctrl)
	if _, err := ctrl.Write(append([]byte{5, 3, 0}, 1, 0, 0, 0, 0, 0, 0)); err != nil {
		t.Fatal(err)
	}
	rep, bound := socksReply(t, ctrl)
	if rep != 0 || bound == nil {
		t.Fatalf("UDP ASSOCIATE reply %d %v", rep, bound)
	}
	relay := bound.(*net.UDPAddr)
	if relay.IP.IsUnspecified() {
		relay.IP = net.IPv4(127, 0, 0, 1)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	pkt := append([]byte{0, 0, 0}, socksAddr(t, target)...)
	pkt = append(pkt, []byte("udp via exit/2")...)
	if _, err := pc.WriteTo(pkt, relay); err != nil {
		t.Fatal(err)
	}
	_ = pc.SetReadDeadline(time.Now().Add(8 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no UDP reply: %v", err)
	}
	if !bytes.HasSuffix(buf[:n], []byte("udp via exit/2")) {
		t.Fatalf("reply %q", buf[:n])
	}
}

// A refusal by the exit reaches the application as a SOCKS5 failure, not as success.
func TestSOCKS5ExitRefusal(t *testing.T) {
	socks := socksRig(t, rigConfig{}) // private targets refused
	c, err := net.DialTimeout("tcp", socks, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	socksHello(t, c)
	if _, err := c.Write(append([]byte{5, 1, 0}, socksAddr(t, "127.0.0.1:9")...)); err != nil {
		t.Fatal(err)
	}
	// With fast open the SOCKS reply may be success followed by a reset; either way no data comes.
	var h [4]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return
	}
	if h[1] != 0 {
		return // refused outright
	}
	if n, _ := io.Copy(io.Discard, c); n != 0 {
		t.Fatalf("%d bytes came back from a refused target", n)
	}
}
