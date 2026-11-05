package profile

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"time"

	quic "github.com/apernet/quic-go"

	"github.com/echofog/echofog-core/internal/v2/carrier/quicx"
)

// OwnConfig describes the TLS server the node would run for a cover: the group and the ALPN the
// cover selected, and a self-signed certificate for its SNI with the leaf key type the cover's
// certificate has (wire.md §3.4). Exact-length padding of the certificate is T2.6b-2/3 and is
// not done here, so the certificate flight differs in length from the cover's; Compare reports
// that as a length difference that padding can fix.
type OwnConfig struct {
	SNI   string
	Group tls.CurveID
	// ALPN is the application protocol the server selects (TCP only; QUIC is always h3).
	ALPN string
	// Key is x509.ECDSA (P-256) or x509.RSA (2048).
	Key x509.PublicKeyAlgorithm
	// NoTickets turns session tickets off (used to build a deliberately different server).
	NoTickets bool
}

// OwnServer is a measurable local TLS or QUIC server built from an OwnConfig. It completes the
// handshake and then idles; it serves no HTTP.
type OwnServer struct {
	Addr  string
	stop  func()
	close sync.Once
}

// Close stops the server.
func (s *OwnServer) Close() { s.close.Do(s.stop) }

// NewSelfSigned issues the self-signed certificate of wire.md §3.4 for sni: no organisation, a
// random serial, a 90-day lifetime.
func NewSelfSigned(sni string, alg x509.PublicKeyAlgorithm) (tls.Certificate, error) {
	var key crypto.Signer
	var err error
	switch alg {
	case x509.RSA:
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	default:
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial.Add(serial, big.NewInt(1)),
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{sni},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// StartOwnTCP starts a TLS 1.3 server on a loopback port.
func StartOwnTCP(cfg OwnConfig) (*OwnServer, error) {
	cert, err := NewSelfSigned(cfg.SNI, cfg.Key)
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{cert},
		SessionTicketsDisabled: cfg.NoTickets,
	}
	if cfg.Group != 0 {
		tc.CurvePreferences = []tls.CurveID{cfg.Group}
	}
	if cfg.ALPN != "" {
		tc.NextProtos = []string{cfg.ALPN}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				tc := tls.Server(c, tc)
				if tc.Handshake() == nil {
					_, _ = io.Copy(io.Discard, tc)
				}
			}()
		}
	}()
	return &OwnServer{Addr: ln.Addr().String(), stop: func() { _ = ln.Close(); wg.Wait() }}, nil
}

// StartOwnQUIC starts a QUIC server with the TLS config of the reality2-quic suite
// (quicx.Reality2TLSConfig) on a loopback port.
func StartOwnQUIC(cfg OwnConfig) (*OwnServer, error) {
	cert, err := NewSelfSigned(cfg.SNI, cfg.Key)
	if err != nil {
		return nil, err
	}
	_, srvTLS := quicx.Reality2TLSConfig(cfg.Group, &cert)
	srvTLS.SessionTicketsDisabled = cfg.NoTickets
	ql, err := quic.ListenAddr("127.0.0.1:0", srvTLS, &quic.Config{EnableDatagrams: true})
	if err != nil {
		return nil, fmt.Errorf("profile: own QUIC server: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ql.Accept(ctx)
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-c.Context().Done()
			}()
		}
	}()
	return &OwnServer{Addr: ql.Addr().String(), stop: func() { cancel(); _ = ql.Close(); wg.Wait() }}, nil
}
