package handshake

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync/atomic"

	utls "github.com/refraction-networking/utls"

	"github.com/apernet/quic-go/quicvarint"
)

// utlsQUICConn adapts uTLS's UQUICConn to the tlsQUICConn interface, translating
// its QUIC event and connection-state types back into the crypto/tls
// equivalents. uTLS is a fork of crypto/tls, so the translation is mechanical.
type utlsQUICConn struct {
	conn *utls.UQUICConn
	// spec is retained because the transport parameters have to be written into
	// the ClientHello extension rather than handed to uTLS directly; see
	// SetTransportParameters.
	spec *utls.ClientHelloSpec
	// initialHandshake buffers Initial-level handshake bytes until a ServerHello
	// is complete enough to read its key_share group; curveID is that group.
	// See recordServerHello (ECHOFOG-PATCH.md, patch 3). curveID is read from
	// ConnectionState, which quic-go calls from a different goroutine than
	// HandleData, so it is atomic; initialHandshake is only touched by the
	// handshake reader.
	initialHandshake []byte
	curveID          atomic.Uint32
}

// maxServerHelloBytes bounds the Initial-level buffer. A ServerHello is a few
// hundred bytes; the buffer drains as soon as its group is known, and only
// accumulates across CRYPTO frames before that.
const maxServerHelloBytes = 16 << 10

var _ tlsQUICConn = (*utlsQUICConn)(nil)

// newUTLSQUICClient creates a QUIC-TLS client emitting the parroted ClientHello.
//
// Session resumption and 0-RTT are disabled: crypto/tls and uTLS each have their
// own SessionState with unexported internals, so a uTLS session cannot be
// converted into the *tls.SessionState the resumption path expects. The events
// are turned off at the source rather than half-supported.
func newUTLSQUICClient(tlsConf *tls.Config) (*utlsQUICConn, error) {
	uConf, err := utlsConfigFromStd(tlsConf)
	if err != nil {
		return nil, err
	}
	spec := chromeQUICClientHelloSpec(tlsConf.NextProtos)

	conn := utls.UQUICClient(&utls.QUICConfig{TLSConfig: uConf}, utls.HelloCustom)
	if err := conn.ApplyPreset(spec); err != nil {
		return nil, fmt.Errorf("applying Chrome ClientHello spec: %w", err)
	}
	return &utlsQUICConn{conn: conn, spec: spec}, nil
}

// utlsConfigFromStd converts a crypto/tls client config into the uTLS
// equivalent.
//
// uTLS ships no converter, so this copies field by field. Any field that cannot
// be carried across is a hard error rather than a silent omission: dropping
// something like VerifyConnection would quietly weaken certificate validation.
func utlsConfigFromStd(c *tls.Config) (*utls.Config, error) {
	if c == nil {
		return &utls.Config{MinVersion: utls.VersionTLS13}, nil
	}
	if c.VerifyConnection != nil {
		// Takes a crypto/tls ConnectionState, which uTLS will never produce.
		return nil, errors.New("quic: tls.Config.VerifyConnection is not supported with ChromeParrot")
	}
	if c.GetConfigForClient != nil || len(c.Certificates) > 0 || c.GetCertificate != nil {
		return nil, errors.New("quic: server-side tls.Config fields are not supported with ChromeParrot")
	}

	uc := &utls.Config{
		Rand:                  c.Rand,
		Time:                  c.Time,
		RootCAs:               c.RootCAs,
		NextProtos:            c.NextProtos,
		ServerName:            c.ServerName,
		InsecureSkipVerify:    c.InsecureSkipVerify,
		VerifyPeerCertificate: c.VerifyPeerCertificate,
		KeyLogWriter:          c.KeyLogWriter,
		// TLS 1.3 only, which QUIC requires regardless.
		MinVersion: utls.VersionTLS13,
		MaxVersion: utls.VersionTLS13,
		// Resumption is off; see newUTLSQUICClient.
		SessionTicketsDisabled: true,
	}

	if c.GetClientCertificate != nil {
		get := c.GetClientCertificate
		uc.GetClientCertificate = func(cri *utls.CertificateRequestInfo) (*utls.Certificate, error) {
			cert, err := get(&tls.CertificateRequestInfo{
				AcceptableCAs:    cri.AcceptableCAs,
				SignatureSchemes: signatureSchemesToStd(cri.SignatureSchemes),
				Version:          cri.Version,
			})
			if err != nil {
				return nil, err
			}
			if cert == nil {
				return &utls.Certificate{}, nil
			}
			return &utls.Certificate{
				Certificate:                 cert.Certificate,
				PrivateKey:                  cert.PrivateKey,
				OCSPStaple:                  cert.OCSPStaple,
				SignedCertificateTimestamps: cert.SignedCertificateTimestamps,
				Leaf:                        cert.Leaf,
			}, nil
		}
	}

	// GREASE ECH is covered by the ClientHello spec; a caller-supplied config list
	// takes precedence.
	if len(c.EncryptedClientHelloConfigList) > 0 {
		uc.EncryptedClientHelloConfigList = c.EncryptedClientHelloConfigList
	}
	return uc, nil
}

func signatureSchemesToStd(in []utls.SignatureScheme) []tls.SignatureScheme {
	out := make([]tls.SignatureScheme, len(in))
	for i, s := range in {
		out[i] = tls.SignatureScheme(s)
	}
	return out
}

func (c *utlsQUICConn) Start(ctx context.Context) error { return c.conn.Start(ctx) }
func (c *utlsQUICConn) Close() error                    { return c.conn.Close() }

func (c *utlsQUICConn) HandleData(level tls.QUICEncryptionLevel, data []byte) error {
	// The client's ServerHello arrives at the Initial encryption level. Read its
	// key_share group before handing the bytes to uTLS, so ConnectionState can
	// report the same CurveID crypto/tls reports on the server side.
	if level == tls.QUICEncryptionLevelInitial && c.curveID.Load() == 0 {
		c.recordServerHello(data)
	}
	return c.conn.HandleData(utlsEncryptionLevel(level), data)
}

// recordServerHello walks buffered handshake messages and remembers the group of
// the first ServerHello that carries a key_share entry. A HelloRetryRequest is
// skipped by serverHelloKeyShareGroup, so the second ServerHello decides.
func (c *utlsQUICConn) recordServerHello(data []byte) {
	if len(c.initialHandshake)+len(data) > maxServerHelloBytes {
		// Not a ServerHello we can read; leave curveID at zero (classical) and
		// stop buffering. uTLS itself will fail the handshake if it is broken.
		c.initialHandshake = nil
		return
	}
	c.initialHandshake = append(c.initialHandshake, data...)
	for len(c.initialHandshake) >= 4 {
		length := int(c.initialHandshake[1])<<16 |
			int(c.initialHandshake[2])<<8 |
			int(c.initialHandshake[3])
		if length > maxServerHelloBytes {
			c.initialHandshake = nil
			return
		}
		if len(c.initialHandshake) < 4+length {
			return
		}
		msg := c.initialHandshake[:4+length]
		c.initialHandshake = c.initialHandshake[4+length:]
		if group, ok := serverHelloKeyShareGroup(msg); ok {
			c.curveID.Store(uint32(group))
			c.initialHandshake = nil
			return
		}
	}
}

// SetTransportParameters installs quic-go's marshalled transport parameters into
// the ClientHello.
//
// uTLS's own SetTransportParameters does not reach the ClientHello when a preset
// is in use, so the bytes must be written into the spec's
// quic_transport_parameters extension. uTLS models that extension as (id, value)
// pairs and marshals it itself, so the blob is split back into pairs here.
// Splitting rather than re-deriving preserves our per-connection ordering.
func (c *utlsQUICConn) SetTransportParameters(params []byte) {
	// Still call through so uTLS's internal copy stays consistent.
	c.conn.SetTransportParameters(params)

	tps, err := splitTransportParameters(params)
	if err != nil {
		// Our own marshaller produced these, so this is unreachable short of a bug.
		panic(fmt.Sprintf("handshake BUG: cannot split marshalled transport parameters: %s", err))
	}
	for _, ext := range c.spec.Extensions {
		if qtp, ok := ext.(*utls.QUICTransportParametersExtension); ok {
			qtp.TransportParameters = tps
			return
		}
	}
	panic("handshake BUG: Chrome ClientHello spec has no quic_transport_parameters extension")
}

// splitTransportParameters parses a marshalled transport parameter blob back into
// individual (id, value) pairs, preserving order.
func splitTransportParameters(b []byte) (utls.TransportParameters, error) {
	var tps utls.TransportParameters
	for len(b) > 0 {
		id, n, err := quicvarint.Parse(b)
		if err != nil {
			return nil, err
		}
		b = b[n:]
		l, n, err := quicvarint.Parse(b)
		if err != nil {
			return nil, err
		}
		b = b[n:]
		if uint64(len(b)) < l {
			return nil, fmt.Errorf("transport parameter 0x%x truncated: want %d bytes, have %d", id, l, len(b))
		}
		val := make([]byte, l)
		copy(val, b[:l])
		b = b[l:]
		tps = append(tps, &utls.FakeQUICTransportParameter{Id: id, Val: val})
	}
	return tps, nil
}

func (c *utlsQUICConn) NextEvent() tls.QUICEvent {
	ev := c.conn.NextEvent()
	out := tls.QUICEvent{
		Level: stdEncryptionLevel(ev.Level),
		Data:  ev.Data,
		Suite: ev.Suite,
	}
	switch ev.Kind {
	case utls.QUICNoEvent:
		out.Kind = tls.QUICNoEvent
	case utls.QUICSetReadSecret:
		out.Kind = tls.QUICSetReadSecret
	case utls.QUICSetWriteSecret:
		out.Kind = tls.QUICSetWriteSecret
	case utls.QUICWriteData:
		out.Kind = tls.QUICWriteData
	case utls.QUICTransportParameters:
		out.Kind = tls.QUICTransportParameters
	case utls.QUICTransportParametersRequired:
		out.Kind = tls.QUICTransportParametersRequired
	case utls.QUICRejectedEarlyData:
		out.Kind = tls.QUICRejectedEarlyData
	case utls.QUICHandshakeDone:
		out.Kind = tls.QUICHandshakeDone
	default:
		// QUICStoreSession and QUICResumeSession carry a *utls.SessionState that
		// cannot be converted. They only fire when EnableSessionEvents is set,
		// which newUTLSQUICClient never does, so reaching this is a bug.
		panic(fmt.Sprintf("handshake BUG: unexpected uTLS QUIC event kind %d", ev.Kind))
	}
	return out
}

func (c *utlsQUICConn) SendSessionTicket(tls.QUICSessionTicketOptions) error {
	return errors.New("quic: SendSessionTicket is server-only and unsupported with ChromeParrot")
}

func (c *utlsQUICConn) StoreSession(*tls.SessionState) error {
	return errors.New("quic: session resumption is unsupported with ChromeParrot")
}

// ExportKeyingMaterial returns RFC 5705 exported keying material from the uTLS
// handshake. ConnectionState below rebuilds a crypto/tls.ConnectionState field by
// field, and its exporter (ekm) is an unexported func that cannot be carried
// across from uTLS, so ConnectionState().TLS.ExportKeyingMaterial would panic on
// a nil ekm. uTLS's own ConnectionState keeps a working exporter (TLS 1.3, and
// the config leaves Renegotiation at RenegotiateNever), so read it there instead.
// cryptoSetup.ExportKeyingMaterial routes to this for the parrot path.
func (c *utlsQUICConn) ExportKeyingMaterial(label string, context []byte, length int) ([]byte, error) {
	cs := c.conn.ConnectionState()
	return cs.ExportKeyingMaterial(label, context, length)
}

func (c *utlsQUICConn) ConnectionState() tls.ConnectionState {
	s := c.conn.ConnectionState()
	return tls.ConnectionState{
		Version:                     s.Version,
		HandshakeComplete:           s.HandshakeComplete,
		DidResume:                   s.DidResume,
		CipherSuite:                 s.CipherSuite,
		CurveID:                     tls.CurveID(c.curveID.Load()),
		NegotiatedProtocol:          s.NegotiatedProtocol,
		NegotiatedProtocolIsMutual:  true,
		ServerName:                  s.ServerName,
		PeerCertificates:            s.PeerCertificates,
		VerifiedChains:              s.VerifiedChains,
		SignedCertificateTimestamps: s.SignedCertificateTimestamps,
		OCSPResponse:                s.OCSPResponse,
		ECHAccepted:                 s.ECHAccepted,
	}
}

func utlsEncryptionLevel(l tls.QUICEncryptionLevel) utls.QUICEncryptionLevel {
	switch l {
	case tls.QUICEncryptionLevelInitial:
		return utls.QUICEncryptionLevelInitial
	case tls.QUICEncryptionLevelEarly:
		return utls.QUICEncryptionLevelEarly
	case tls.QUICEncryptionLevelHandshake:
		return utls.QUICEncryptionLevelHandshake
	case tls.QUICEncryptionLevelApplication:
		return utls.QUICEncryptionLevelApplication
	default:
		panic(fmt.Sprintf("handshake BUG: unknown encryption level %d", l))
	}
}

func stdEncryptionLevel(l utls.QUICEncryptionLevel) tls.QUICEncryptionLevel {
	switch l {
	case utls.QUICEncryptionLevelInitial:
		return tls.QUICEncryptionLevelInitial
	case utls.QUICEncryptionLevelEarly:
		return tls.QUICEncryptionLevelEarly
	case utls.QUICEncryptionLevelHandshake:
		return tls.QUICEncryptionLevelHandshake
	case utls.QUICEncryptionLevelApplication:
		return tls.QUICEncryptionLevelApplication
	default:
		panic(fmt.Sprintf("handshake BUG: unknown uTLS encryption level %d", l))
	}
}
