package r2flight

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
)

func cert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type rig struct {
	ln        *reality2.TCPListener
	mask      *ecdh.PrivateKey
	addr      string
	covers    atomic.Int64
	coverAddr string
	clock     atomic.Int64
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{}
	r.clock.Store(time.Now().UnixNano())
	cover, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert(t)}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cover.Close() })
	go func() {
		for {
			c, err := cover.Accept()
			if err != nil {
				return
			}
			r.covers.Add(1)
			go func() { defer c.Close(); _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	r.coverAddr = cover.Addr().String()
	r.mask, err = ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r.addr = raw.Addr().String()
	own := cert(t)
	r.ln, err = reality2.ListenTCP(raw, reality2.TCPListenerConfig{
		Cover:     reality2.TCPCover{Group: tls.X25519MLKEM768, ALPN: "h2", Cert: &own},
		Masks:     []reality2.MaskKey{{Private: r.mask}},
		Addrs:     []string{r.addr},
		CoverAddr: cover.Addr().String(),
		Now:       func() time.Time { return time.Unix(0, r.clock.Load()) },
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.ln.Close() })
	return r
}

// send writes b to addr and returns the first bytes that come back.
func send(t *testing.T, addr string, b []byte) []byte {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write(b)
	require.NoError(t, err)
	_ = c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	got := make([]byte, 16)
	n, _ := io.ReadFull(c, got)
	return got[:n]
}

// Both answers start with a handshake record, so the cover is counted: a foreign token is forwarded
// to it, a fresh valid one is answered by the node itself and never reaches it.
func TestAFreshFlightIsAdmittedAndAForeignOneGoesToTheCover(t *testing.T) {
	r := newRig(t)
	now := time.Unix(0, r.clock.Load())

	valid, err := Flight(r.mask.PublicKey().Bytes(), r.addr, "cover.example", now)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(valid), 5)
	assert.Equal(t, byte(0x16), valid[0], "a TLS handshake record")

	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	foreign, err := Flight(other.PublicKey().Bytes(), r.addr, "cover.example", now)
	require.NoError(t, err)

	got := send(t, r.addr, foreign)
	require.GreaterOrEqual(t, len(got), 5)
	assert.EqualValues(t, 1, r.covers.Load(), "the foreign flight went to the cover")

	got = send(t, r.addr, valid)
	require.GreaterOrEqual(t, len(got), 5, "the node answered a fresh valid flight itself")
	assert.Equal(t, byte(0x16), got[0], "with its own ServerHello")
	assert.EqualValues(t, 1, r.covers.Load(), "and did not forward it to the cover")
	assert.NotEqual(t, valid, foreign)
}

func TestTwoFlightsDiffer(t *testing.T) {
	r := newRig(t)
	now := time.Unix(0, r.clock.Load())
	a, err := Flight(r.mask.PublicKey().Bytes(), r.addr, "cover.example", now)
	require.NoError(t, err)
	b, err := Flight(r.mask.PublicKey().Bytes(), r.addr, "cover.example", now)
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "a fresh ephemeral key every time, or the replay cache would eat the second")
}
