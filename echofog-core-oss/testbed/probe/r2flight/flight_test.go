package r2flight

import (
	"context"
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
	ln     *reality2.TCPListener
	mask   *ecdh.PrivateKey
	addr   string
	covers atomic.Int64
	clock  atomic.Int64
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

func send(t *testing.T, addr string, b []byte) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write(b)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
}

func TestAFreshFlightIsAdmittedAndAForeignOneGoesToTheCover(t *testing.T) {
	r := newRig(t)
	now := time.Unix(0, r.clock.Load())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admitted := make(chan struct{}, 4)
	go func() {
		for {
			c, err := r.ln.Accept(ctx)
			if err != nil {
				return
			}
			admitted <- struct{}{}
			_ = c.Close()
		}
	}()
	valid, err := Flight(r.mask.PublicKey().Bytes(), r.addr, "cover.example", now)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(valid), 5)
	assert.Equal(t, byte(0x16), valid[0], "a TLS handshake record")

	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	foreign, err := Flight(other.PublicKey().Bytes(), r.addr, "cover.example", now)
	require.NoError(t, err)

	send(t, r.addr, foreign)
	select {
	case <-admitted:
		t.Fatal("a flight made for another key was admitted")
	default:
	}
	assert.EqualValues(t, 1, r.covers.Load(), "the foreign flight went to the cover")

	send(t, r.addr, valid)
	select {
	case <-admitted:
	case <-time.After(3 * time.Second):
		t.Fatal("a fresh valid flight was not admitted")
	}
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
