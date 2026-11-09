package reality2

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// TCPDialerConfig configures a TCPDialer.
type TCPDialerConfig struct {
	// Hello is the browser ClientHello the connection imitates; the zero value is the current
	// stable Chrome (utls.HelloChrome_Auto), as wire.md §4.1 requires.
	Hello utls.ClientHelloID
	// Now is the injectable clock for token timestamps; nil means time.Now.
	Now func() time.Time
}

// TCPDialer dials one reality2-tcp address of a verified card (wire.md §4.1).
type TCPDialer struct {
	cfg TCPDialerConfig
}

// NewTCPDialer builds the suite's dialer.
func NewTCPDialer(cfg TCPDialerConfig) *TCPDialer {
	if cfg.Hello == (utls.ClientHelloID{}) {
		cfg.Hello = utls.HelloChrome_Auto
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &TCPDialer{cfg: cfg}
}

var _ carrier.Dialer = (*TCPDialer)(nil)

// Dial opens one TCP connection, runs a Chrome-shaped TLS 1.3 handshake whose session id is a
// REALITY v2 token, and returns the byte-stream mux over it. The proxy hook of the request, when
// set, opens the TCP socket (T2.1, SOCKS5).
func (d *TCPDialer) Dial(ctx context.Context, req carrier.DialRequest) (mux.Carrier, error) {
	a := req.Addr
	if a == nil || a.GetSuite() != objects.SuiteReality2TCP {
		return nil, fmt.Errorf("%w: address is not reality2-tcp", ErrCard)
	}
	if req.Card == nil {
		return nil, fmt.Errorf("%w: no card", ErrCard)
	}
	if a.GetHost() == "" || a.GetPort() == 0 {
		return nil, fmt.Errorf("%w: no host or port", ErrCard)
	}
	// Validate the card before touching the network.
	if _, _, _, err := cardParams(req.Card, a); err != nil {
		return nil, err
	}
	addr := AddressString(a.GetHost(), a.GetPort())

	// A path that swallows the first big segments is answered with a retry on a limited MSS, on
	// the retry only (T3.12, wire.md section 2.4).
	return carrier.WithNarrowRetry(ctx, req, func(ctx context.Context, req carrier.DialRequest) (mux.Carrier, error) {
		var raw net.Conn
		var err error
		if req.DialContext != nil {
			raw, err = req.DialContext(ctx, "tcp", addr)
		} else {
			var nd net.Dialer
			raw, err = nd.DialContext(ctx, "tcp", addr)
		}
		if err != nil {
			return nil, fmt.Errorf("reality2: dial %s: %w", addr, err)
		}
		return d.DialOver(ctx, raw, req.Card, a, addr)
	})
}

// cardParams extracts the masking key, the SNI and the shape profile of a card address.
func cardParams(card *objects.Card, a *pb.Addr) (maskPub []byte, sni string, prof *shape.Profile, err error) {
	if card == nil {
		return nil, "", nil, fmt.Errorf("%w: no card", ErrCard)
	}
	maskPub, err = maskPublic(card, a.GetMask())
	if err != nil {
		return nil, "", nil, err
	}
	sp := objects.SuiteParams(a)
	sni = sp.GetSni()
	if sni == "" {
		return nil, "", nil, fmt.Errorf("%w: no SNI in the card", ErrCard)
	}
	prof, err = shape.Parse(sp.GetShape())
	if err != nil {
		return nil, "", nil, fmt.Errorf("%w: shape profile: %v", ErrCard, err)
	}
	return maskPub, sni, prof, nil
}

// DialOver runs the reality2-tcp client over raw, an already open byte stream (ws2 passes the
// WebSocket here, wire.md §4.4); it takes ownership of raw and closes it on failure. tokenAddr is
// the address string the token binds to.
func (d *TCPDialer) DialOver(ctx context.Context, raw net.Conn, card *objects.Card, a *pb.Addr, tokenAddr string) (mux.Carrier, error) {
	addr := tokenAddr
	maskPub, sni, prof, err := cardParams(card, a)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	// The context bounds the handshake; a cancelled context closes the socket.
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()

	ucfg := &utls.Config{ServerName: sni, InsecureSkipVerify: true}
	uc := utls.UClient(raw, ucfg, d.cfg.Hello)
	if err := PrepareTCPHello(uc, maskPub, addr, d.cfg.Now); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("reality2: token: %w", err)
	}
	if err := uc.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("reality2: handshake with %s: %w", addr, err)
	}
	if !stop() { // the context fired after the handshake: the socket may be closed
		_ = raw.Close()
		return nil, ctx.Err()
	}
	// The Chrome preset enables TLS 1.2 renegotiation, and utls then withholds the exporter even
	// from a TLS 1.3 session, which has no renegotiation. Switch it off before reading the state.
	ucfg.Renegotiation = utls.RenegotiateNever
	st := uc.ConnectionState()
	if st.Version != utls.VersionTLS13 {
		_ = raw.Close()
		return nil, errors.New("reality2: the server did not negotiate TLS 1.3")
	}
	// wire.md §8.5: the group the server selected, from its ServerHello, decides sealed mode.
	sh := uc.HandshakeState.ServerHello
	if sh == nil {
		_ = raw.Close()
		return nil, errors.New("reality2: no ServerHello after the handshake")
	}
	sealed := !isPostQuantumGroup(uint16(sh.ServerShare.Group))
	cb, err := channelBinding(st.ExportKeyingMaterial)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return newTCPCarrier(uc, true, cb, sealed, prof), nil
}

// PrepareTCPHello puts a REALITY v2 token into the 32-byte session id of the Chrome-shaped hello
// of uc (wire.md §4.1, §3.2): the hello is built with a zero session id, CH0 is taken from it, and
// the token replaces the zeros. The second marshal must give the same CH0 or the token would not
// bind. It must run before the handshake.
func PrepareTCPHello(uc *utls.UConn, maskPub []byte, addr string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	pub, err := ecdh.X25519().NewPublicKey(maskPub)
	if err != nil {
		return fmt.Errorf("masking key: %w", err)
	}
	if err := uc.BuildHandshakeState(); err != nil {
		return err
	}
	hello := uc.HandshakeState.Hello
	hello.SessionId = make([]byte, 32)
	if err := uc.MarshalClientHello(); err != nil {
		return err
	}
	parsed, err := ParseHelloTCP(hello.Raw)
	if err != nil {
		return fmt.Errorf("own hello: %w", err)
	}
	ks := uc.HandshakeState.State13.KeyShareKeys
	if ks == nil || ks.Ecdhe == nil || !bytes.Equal(ks.Ecdhe.PublicKey().Bytes(), parsed.C) {
		return errors.New("the hello's X25519 share has no private key to bind the token to")
	}
	z, err := ks.Ecdhe.ECDH(pub)
	if err != nil {
		return fmt.Errorf("token key: %w", err)
	}
	tok, err := SealToken(TokenKey(z, maskPub, ks.Ecdhe.PublicKey().Bytes(), addr), HelloAAD(parsed.CH0), uint32(now().Unix()))
	if err != nil {
		return err
	}
	hello.SessionId = tok
	if err := uc.MarshalClientHello(); err != nil {
		return err
	}
	again, err := ParseHelloTCP(hello.Raw)
	if err != nil || !bytes.Equal(again.CH0, parsed.CH0) {
		return errors.New("the hello changed between the two marshals")
	}
	return nil
}
