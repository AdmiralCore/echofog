package scenarios

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/censor"
)

var (
	fakeSrv  = netip.MustParseAddrPort("203.0.113.40:443")
	fakeSrv2 = netip.MustParseAddrPort("203.0.113.41:4433")
)

func selfSigned(t testing.TB, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// tlsEcho serves TLS on loopback and echoes every byte; the tap relays to it.
func tlsEcho(t testing.TB, names ...string) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{selfSigned(t, names...)}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// A pcap written by the writer is read back by the censor's analyzer as the packets that were
// written: protocol, ports, bytes each way.
func TestPcapWriterRoundTripsUDP(t *testing.T) {
	pw, wait := newCapture()
	cli := netip.AddrPortFrom(fakeClientIP, 50000)
	at := time.Now()
	require.NoError(t, pw.UDP(at, cli, fakeSrv, bytes.Repeat([]byte{1}, 100)))
	require.NoError(t, pw.UDP(at.Add(time.Millisecond), fakeSrv, cli, bytes.Repeat([]byte{2}, 300)))
	require.NoError(t, pw.UDP(at.Add(2*time.Millisecond), cli, fakeSrv, bytes.Repeat([]byte{3}, 50)))
	flows, err := wait()
	require.NoError(t, err)
	require.Len(t, flows, 1)
	f := flows[0]
	assert.Equal(t, "udp", f.Proto)
	assert.Equal(t, cli, f.Client)
	assert.Equal(t, fakeSrv, f.Server)
	assert.Equal(t, 3, f.Packets)
	assert.Equal(t, 300, f.PayDn)
	assert.Equal(t, 100, f.UDPFirst)
	assert.Equal(t, 2, f.UDPPackets)
	// IP bytes: header 20 + UDP 8 + payload
	assert.Equal(t, 2*28+150, f.BytesUp)
	assert.Equal(t, 28+300, f.BytesDn)
}

func TestPcapWriterRoundTripsATCPConnection(t *testing.T) {
	pw, wait := newCapture()
	cli := netip.AddrPortFrom(fakeClientIP, 51000)
	at := time.Now()
	require.NoError(t, pw.TCP(at, cli, fakeSrv, 1000, 0, tcpSYN, nil))
	require.NoError(t, pw.TCP(at.Add(time.Millisecond), fakeSrv, cli, 5000, 1001, tcpSYN|tcpACK, nil))
	require.NoError(t, pw.TCP(at.Add(2*time.Millisecond), cli, fakeSrv, 1001, 5001, tcpACK, nil))
	require.NoError(t, pw.TCP(at.Add(3*time.Millisecond), cli, fakeSrv, 1001, 5001, tcpACK|tcpPSH, []byte("GET / HTTP/1.1\r\n\r\n")))
	require.NoError(t, pw.TCP(at.Add(4*time.Millisecond), fakeSrv, cli, 5001, 1019, tcpACK|tcpPSH, bytes.Repeat([]byte("x"), 700)))
	require.NoError(t, pw.TCP(at.Add(5*time.Millisecond), cli, fakeSrv, 1019, 5701, tcpACK|tcpFIN, nil))
	flows, err := wait()
	require.NoError(t, err)
	require.Len(t, flows, 1)
	f := flows[0]
	assert.Equal(t, "tcp", f.Proto)
	assert.Equal(t, cli, f.Client, "the side that sent the SYN is the client")
	assert.Equal(t, fakeSrv, f.Server)
	assert.Equal(t, 6, f.Packets)
	assert.Equal(t, 700, f.PayDn)
	assert.Equal(t, "HTTP", f.Looks)
	assert.Equal(t, 18, f.FirstSize)
	require.Len(t, f.Pkts, 6)
	assert.Equal(t, uint32(1001), f.Pkts[2].Seq)
	assert.True(t, f.Pkts[0].Flags&tcpSYN != 0)
}

// The TCP tap carries a real TLS connection through, writes the handshake with the SYN, SYN-ACK and
// ACK first, and what the analyzer reads is the ClientHello that was sent: its SNI, its records.
func TestTCPTapShowsTheClientHelloToTheAnalyzer(t *testing.T) {
	target := tlsEcho(t, "cover.example")
	pw, wait := newCapture()
	tap, err := NewTCPTap(target, fakeSrv, pw)
	require.NoError(t, err)

	c, err := net.Dial("tcp", tap.Addr())
	require.NoError(t, err)
	tc := tls.Client(c, &tls.Config{ServerName: "cover.example", InsecureSkipVerify: true}) // #nosec G402 -- test certificate
	require.NoError(t, tc.Handshake())
	payload := bytes.Repeat([]byte("abcdefgh"), 4096) // 32 KiB: more than one segment each way
	go func() { _, _ = tc.Write(payload) }()
	got := make([]byte, len(payload))
	_, err = io.ReadFull(tc, got)
	require.NoError(t, err)
	require.Equal(t, payload, got)
	require.NoError(t, tc.Close())
	require.NoError(t, tap.Close())
	flows, err := wait()
	require.NoError(t, err)

	require.Len(t, flows, 1)
	f := flows[0]
	assert.Equal(t, "tcp", f.Proto)
	assert.Equal(t, fakeClientIP, f.Client.Addr())
	assert.Equal(t, fakeSrv, f.Server)
	assert.Equal(t, c.LocalAddr().(*net.TCPAddr).Port, int(f.Client.Port()), "the client port is the real one")
	assert.Equal(t, "TLS", f.Looks)
	assert.Equal(t, "cover.example", f.SNI)
	require.NotNil(t, f.Hello)
	assert.GreaterOrEqual(t, f.PayDn, len(payload), "the echo and the handshake came down")
	assert.Greater(t, f.BytesUp, len(payload))
	assert.NotEmpty(t, f.Records, "the TLS records of the flow are read")
	require.GreaterOrEqual(t, len(f.Pkts), 6)
	assert.True(t, f.Pkts[0].Flags&tcpSYN != 0 && f.Pkts[0].Up, "the first packet is the client's SYN")
	assert.True(t, f.Pkts[1].Flags&tcpSYN != 0 && !f.Pkts[1].Up, "the second is the SYN-ACK")
	for _, p := range f.Pkts {
		assert.LessOrEqual(t, p.Payload, 1460, "a segment fits the fake MSS")
	}
}

func TestUDPTapRelaysAndWritesWhatItSaw(t *testing.T) {
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo(buf[:n], from)
		}
	}()
	pw, wait := newCapture()
	tap, err := NewUDPTap(echo.LocalAddr().String(), fakeSrv2, pw)
	require.NoError(t, err)

	c, err := net.Dial("udp", tap.Addr())
	require.NoError(t, err)
	msg := bytes.Repeat([]byte{0xab}, 200)
	first := append([]byte{0xc0}, msg...)
	for i := 0; i < 5; i++ {
		d := msg
		if i == 0 {
			d = first
		}
		_, err = c.Write(d)
		require.NoError(t, err)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 2048)
		n, err := c.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, len(d), n)
	}
	assert.Equal(t, first, tap.FirstClientDatagram())
	_ = c.Close()
	require.NoError(t, tap.Close())
	flows, err := wait()
	require.NoError(t, err)
	require.Len(t, flows, 1)
	f := flows[0]
	assert.Equal(t, "udp", f.Proto)
	assert.Equal(t, fakeClientIP, f.Client.Addr())
	assert.Equal(t, fakeSrv2, f.Server)
	assert.Equal(t, 10, f.Packets)
	assert.Equal(t, len(first)+4*len(msg), f.BytesUp-5*28)
	assert.Equal(t, 5*len(msg)+1, f.PayDn)
	assert.Equal(t, len(first), f.UDPFirst)
}

var _ = censor.LabelCarrier
