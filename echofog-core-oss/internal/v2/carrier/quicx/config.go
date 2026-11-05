package quicx

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"

	quic "github.com/apernet/quic-go"
)

const (
	// alpnH3 is the application protocol of the inner TLS (wire.md §5.1).
	alpnH3 = "h3"

	// PathMTU is the largest UDP payload on an Ethernet path: 1500 minus the
	// IPv6 (40) and UDP (8) headers. It is also quic-go's default ceiling for
	// max_udp_payload_size.
	PathMTU = 1452

	// Noise3DataOverhead is the noise3 data-packet overhead (wire.md §2.6).
	Noise3DataOverhead = 22

	// minQUICPayload is the smallest legal QUIC packet (RFC 9000); quic-go
	// rejects smaller values.
	minQUICPayload = 1200
)

// Noise3TLSConfig returns the inner TLS configs of a noise3 QUIC path (wire.md §5.1): a
// self-signed Ed25519 certificate with an empty subject and a random serial on the server, ALPN
// h3, and X25519MLKEM768 as the only key exchange group on both sides. Certificates are not
// verified: authentication is the Noise handshake, bound to this TLS session by
// ChannelBinding.
func Noise3TLSConfig() (client, server *tls.Config, err error) {
	cert, err := selfSignedEd25519()
	if err != nil {
		return nil, nil, err
	}
	client = &tls.Config{
		MinVersion:         tls.VersionTLS13,
		CurvePreferences:   []tls.CurveID{tls.X25519MLKEM768},
		NextProtos:         []string{alpnH3},
		InsecureSkipVerify: true, // wire.md §5.1: the certificate is not authenticated
	}
	server = &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768},
		NextProtos:       []string{alpnH3},
		Certificates:     []tls.Certificate{cert},
	}
	return client, server, nil
}

// Reality2TLSConfig returns the inner TLS configs of a reality2-quic path (wire.md §3.4,
// §5.1). group is the key exchange group of the cover: the ServerHello must carry the same
// one, so the value comes from the cover profile (T2.6b) and is set as the only group. A zero
// group leaves the preference at the library default. serverCert is the cover's certificate
// (also mirrored from the profile); nil leaves it for the caller to install.
//
// Certificates are not verified on the client (wire.md §5.1), and the client's SNI is the
// cover's, so the caller sets ServerName on the returned client config.
func Reality2TLSConfig(group tls.CurveID, serverCert *tls.Certificate) (client, server *tls.Config) {
	var curves []tls.CurveID
	if group != 0 {
		curves = []tls.CurveID{group}
	}
	client = &tls.Config{
		MinVersion:         tls.VersionTLS13,
		CurvePreferences:   curves,
		NextProtos:         []string{alpnH3},
		InsecureSkipVerify: true, // wire.md §5.1: the certificate is not authenticated
	}
	server = &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: curves,
		NextProtos:       []string{alpnH3},
	}
	if serverCert != nil {
		server.Certificates = []tls.Certificate{*serverCert}
	}
	return client, server
}

// Noise3QUICConfig returns the QUIC config of the noise3 suite (wire.md §5.1): idle timeout
// 60 s, no QUIC keepalive (L2 PING keeps the path alive), DATAGRAM frames on, at most 256
// bidirectional streams, no unidirectional streams, no 0-RTT.
//
// maxPadding is the profile's maximum padding per data packet. The datagram the inner QUIC
// stack may send or receive is PathMTU minus Noise3DataOverhead minus maxPadding, so that an
// outer noise3 datagram still fits the path MTU. Path MTU discovery is off: it would grow
// packets back to the path MTU and undo that budget. A profile that leaves less than a minimal
// QUIC packet is an error, not something to clamp.
func Noise3QUICConfig(maxPadding int) (*quic.Config, error) {
	if maxPadding < 0 {
		return nil, fmt.Errorf("quicx: negative maximum padding %d", maxPadding)
	}
	inner := PathMTU - Noise3DataOverhead - maxPadding
	if inner < minQUICPayload {
		return nil, fmt.Errorf("quicx: maximum padding %d leaves %d-byte QUIC datagrams, want at least %d",
			maxPadding, inner, minQUICPayload)
	}
	return &quic.Config{
		MaxIdleTimeout:          60 * time.Second,
		KeepAlivePeriod:         0,
		MaxIncomingStreams:      256,
		MaxIncomingUniStreams:   -1, // mux.Carrier has no unidirectional streams; see doc.go
		EnableDatagrams:         true,
		MaxDatagramFrameSize:    int64(inner),
		InitialPacketSize:       uint16(inner),
		MaxUDPPayloadSize:       uint16(inner),
		DisablePathMTUDiscovery: true,
		Allow0RTT:               false,
	}, nil
}

// Reality2QUICConfig returns the QUIC config of a reality2-quic client: ChromeParrot, so the
// handshake, transport parameters and Initial packets look like Chrome's (the cover is mirrored
// from a real Chrome). It is client-only; a listener ignores ChromeParrot and uses a plain
// quic.Config with datagrams enabled.
func Reality2QUICConfig() *quic.Config {
	return &quic.Config{
		ChromeParrot:    true,
		EnableDatagrams: true,
	}
}

// selfSignedEd25519 builds the noise3 inner certificate: Ed25519, an empty subject, and a
// random serial (wire.md §5.1).
func selfSignedEd25519() (tls.Certificate, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("quicx: generating Ed25519 key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("quicx: generating serial: %w", err)
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{}, // empty subject, wire.md §5.1
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("quicx: creating certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("quicx: parsing certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, nil
}
