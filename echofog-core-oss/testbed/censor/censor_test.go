package censor

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The decryption a censor does: a real QUIC Initial, as quic-go sends it, opens with the keys of
// its own connection ID and names its SNI.
func TestRealQUICInitialDecryptsAndNamesTheSNI(t *testing.T) {
	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer sink.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		_, _ = quic.DialAddr(ctx, sink.LocalAddr().String(), &tls.Config{ServerName: "cover.example", NextProtos: []string{"h3"}}, nil)
	}()

	got := quicInitial{crypto: map[int][]byte{}}
	buf := make([]byte, 2048)
	for i := 0; i < 3; i++ {
		_ = sink.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := sink.ReadFrom(buf)
		require.NoError(t, err)
		q := openInitials(buf[:n])
		if q.decrypted {
			got.decrypted = true
			for o, d := range q.crypto {
				got.crypto[o] = d
			}
		}
		if ch, ok := parseClientHello(assembleCrypto(got.crypto), true); ok {
			assert.Equal(t, "cover.example", ch.SNI)
			assert.Equal(t, []string{"h3"}, ch.ALPN)
			assert.True(t, got.decrypted)
			return
		}
	}
	t.Fatal("no ClientHello read from the Initials")
}

// Noise that only borrows QUIC's first bytes does not decrypt.
func TestQUICShapedNoiseDoesNotDecrypt(t *testing.T) {
	b := make([]byte, 1200)
	_, _ = rand.Read(b)
	b[0] = 0xc3
	b[1], b[2], b[3], b[4] = 0, 0, 0, 1
	b[5] = 8
	q := openInitials(b)
	assert.True(t, q.isLongHeader)
	assert.False(t, q.decrypted)
}

func TestFullyEncryptedRule(t *testing.T) {
	random := make([]byte, 600)
	_, _ = rand.Read(random)
	flagged, _ := fullyEncrypted(random)
	assert.True(t, flagged, "random bytes are what the rule catches")

	for name, p := range map[string][]byte{
		"TLS":        append([]byte{0x16, 0x03, 0x01, 0x02, 0x00}, random[:300]...),
		"HTTP":       []byte("GET / HTTP/1.1\r\nHost: example.org\r\n\r\n"),
		"zeros":      make([]byte, 600),
		"text first": append([]byte("abcdef"), random[:300]...),
	} {
		flagged, why := fullyEncrypted(p)
		assert.False(t, flagged, "%s must be exempt (%s)", name, why)
	}
}

// A ClientHello from Go's crypto/tls has no GREASE: the fingerprint of a library, not a browser.
func TestClientHelloFromGoHasNoGrease(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	go func() {
		_ = tls.Client(c1, &tls.Config{ServerName: "site.example", InsecureSkipVerify: true}).Handshake()
	}()
	buf := make([]byte, 4096)
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c2.Read(buf)
	require.NoError(t, err)
	hs, ok := tlsHandshake(buf[:n])
	require.True(t, ok)
	ch, ok := parseClientHello(hs, false)
	require.True(t, ok)
	assert.Equal(t, "site.example", ch.SNI)
	assert.False(t, ch.Grease)
	assert.Contains(t, ch.Print, "t13d")
}

func TestSNIThatTheAddressDoesNotHostIsAtRisk(t *testing.T) {
	srv := netip.MustParseAddrPort("203.0.113.15:443")
	flows := []Flow{{Server: srv, Looks: "TLS", SNI: "www.microsoft.com", Hello: &clientHello{Grease: true}},
		{Server: srv, Looks: "TLS", SNI: "cover.lab", Hello: &clientHello{Grease: true}}}
	CheckSNI(flows, map[netip.Addr][]string{srv.Addr(): {"cover.lab"}})
	assert.Contains(t, flows[0].Verdict(), "AT RISK")
	assert.Equal(t, "blends in", flows[1].Verdict())
}
