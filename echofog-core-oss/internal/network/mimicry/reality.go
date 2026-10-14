package mimicry

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/crypto/hkdf"
)

const (
	// ChannelBindingLabel and ChannelBindingLen produce the per-connection secret (TLS exporter)
	// that the peer handshake signs, so a relay terminating the TLS in the middle cannot reuse a
	// captured signature on another connection.
	ChannelBindingLabel = "EXPORTER-echofog-peer-handshake"
	ChannelBindingLen   = 32
)

const (
	// The authorization token lives in the ClientHello's 32-byte legacy_session_id. It is
	// nonce(12) || AES-GCM(timestamp uint32, 4 bytes) + tag(16) = 32 bytes, which is exactly the
	// size of a normal TLS 1.3 compatibility session id.
	tokenNonceLen   = 12
	tokenTSLen      = 4
	tokenTagLen     = 16
	StealthTokenLen = tokenNonceLen + tokenTSLen + tokenTagLen // 32

	// ReplayWindowSeconds is how far the token timestamp may drift from now.
	ReplayWindowSeconds = 300
)

var (
	ErrInvalidStealthToken = errors.New("mimicry: invalid stealth authentication token")
	ErrReplayDetected      = errors.New("mimicry: replay attack or expired timestamp detected")
)

// DeriveAEAD derives an AES-GCM cipher from the AuthKey using HKDF.
func DeriveAEAD(authKey []byte) (cipher.AEAD, error) {
	if len(authKey) == 0 {
		return nil, errors.New("mimicry: auth key cannot be empty")
	}
	kdf := hkdf.New(sha256.New, authKey, nil, []byte("echofog-reality-stealth-aead-v2"))
	key := make([]byte, 32)
	if _, err := io.ReadFull(kdf, key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// GenerateStealthToken creates a 32-byte token carrying the current time, to put in the
// ClientHello session id.
func GenerateStealthToken(aead cipher.AEAD) ([]byte, error) {
	token := make([]byte, StealthTokenLen)
	nonce := token[:tokenNonceLen]
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	var ts [tokenTSLen]byte
	binary.BigEndian.PutUint32(ts[:], uint32(time.Now().Unix()))
	// seal appends ciphertext+tag after the nonce, filling token[tokenNonceLen:]
	aead.Seal(token[:tokenNonceLen], nonce, ts[:], nil)
	return token, nil
}

// VerifyStealthToken decrypts a session id, checks the clock drift, and guards against replay.
func VerifyStealthToken(aead cipher.AEAD, token []byte) error {
	return verifyStealthToken(aead, token, sharedReplayCache, nowUnix())
}

func verifyStealthToken(aead cipher.AEAD, token []byte, replay *replayCache, now int64) error {
	if len(token) != StealthTokenLen {
		return ErrInvalidStealthToken
	}
	nonce := token[:tokenNonceLen]
	plain, err := aead.Open(nil, nonce, token[tokenNonceLen:], nil)
	if err != nil || len(plain) != tokenTSLen {
		return ErrInvalidStealthToken
	}
	ts := int64(binary.BigEndian.Uint32(plain))
	if diff := now - ts; diff > ReplayWindowSeconds || diff < -ReplayWindowSeconds {
		return ErrReplayDetected
	}
	if replay != nil && replay.seenBefore(string(token), now) {
		return ErrReplayDetected
	}
	return nil
}

// ClientHandshake dials with a genuine Google Chrome TLS 1.3 ClientHello (uTLS), hiding the
// authorization token in the session id, so the fingerprint on the wire is Chrome's and the token
// is invisible. It returns the connection and the channel binding of its TLS session.
func ClientHandshake(rawConn net.Conn, cfg *MimicryConfig) (net.Conn, []byte, error) {
	serverName := cfg.ServerName
	if serverName == "" {
		serverName = "www.microsoft.com"
	}
	// InsecureSkipVerify: the real peer authentication happens afterwards and is bound to this
	// TLS session, so a forged certificate cannot help a man in the middle. Verifying here would
	// also break operators who run a self-signed certificate.
	uCfg := &utls.Config{ServerName: serverName, InsecureSkipVerify: true}
	uconn := utls.UClient(rawConn, uCfg, utls.HelloChrome_Auto)

	aead, err := DeriveAEAD(cfg.AuthKey)
	if err != nil {
		_ = uconn.Close()
		return nil, nil, err
	}
	token, err := GenerateStealthToken(aead)
	if err != nil {
		_ = uconn.Close()
		return nil, nil, err
	}
	// build the Chrome ClientHello, then replace its random session id with our token and
	// re-marshal; the token is 32 bytes, the size Chrome uses, so nothing on the wire stands out
	if err := uconn.BuildHandshakeState(); err != nil {
		_ = uconn.Close()
		return nil, nil, fmt.Errorf("mimicry: build client hello: %w", err)
	}
	uconn.HandshakeState.Hello.SessionId = token
	if err := uconn.MarshalClientHello(); err != nil {
		_ = uconn.Close()
		return nil, nil, fmt.Errorf("mimicry: marshal client hello: %w", err)
	}

	if err := uconn.Handshake(); err != nil {
		_ = uconn.Close()
		return nil, nil, fmt.Errorf("mimicry: client TLS handshake failed: %w", err)
	}

	// The Chrome parrot advertises secure renegotiation, which makes uTLS gate off
	// ExportKeyingMaterial. TLS 1.3 has no renegotiation anyway, so clear the flag on our own
	// config before reading the channel binding; it does not change anything already on the wire.
	uCfg.Renegotiation = utls.RenegotiateNever
	cs := uconn.ConnectionState()
	if cs.Version != utls.VersionTLS13 {
		_ = uconn.Close()
		return nil, nil, fmt.Errorf("mimicry: negotiated TLS %x, need 1.3", cs.Version)
	}
	binding, err := cs.ExportKeyingMaterial(ChannelBindingLabel, nil, ChannelBindingLen)
	if err != nil {
		_ = uconn.Close()
		return nil, nil, fmt.Errorf("mimicry: channel binding: %w", err)
	}
	return uconn, binding, nil
}

// ServerHandshake reads the ClientHello, and if its session id is a valid token, completes TLS
// itself and returns (tlsConn, true, binding). For anybody else it returns a connection that
// replays the bytes already read and false, so HandleActiveProbe can hand the whole connection,
// starting with that ClientHello, to the real site: the stranger then does genuine TLS with the
// real site and sees its real certificate. Our own TLS is never shown to a stranger.
func ServerHandshake(rawConn net.Conn, tlsCfg *tls.Config, cfg *MimicryConfig) (net.Conn, bool, []byte, error) {
	rec, ok := readClientHelloRecord(rawConn)
	passOn := &prefixConn{Conn: rawConn, prefix: rec}
	if !ok {
		return passOn, false, nil, nil
	}
	sid, has := sessionIDFromClientHello(rec)
	if !has || len(sid) != StealthTokenLen {
		return passOn, false, nil, nil
	}
	aead, err := DeriveAEAD(cfg.AuthKey)
	if err != nil {
		return nil, false, nil, err
	}
	if VerifyStealthToken(aead, sid) != nil {
		return passOn, false, nil, nil
	}

	// one of ours: complete TLS with our certificate (the client skips verification)
	tlsConn := tls.Server(passOn, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		return nil, false, nil, err
	}
	scs := tlsConn.ConnectionState()
	binding, err := scs.ExportKeyingMaterial(ChannelBindingLabel, nil, ChannelBindingLen)
	if err != nil {
		return nil, false, nil, fmt.Errorf("mimicry: channel binding: %w", err)
	}
	return tlsConn, true, binding, nil
}

// prefixConn returns prefix before reading from Conn.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// HandleActiveProbe hands a stranger's connection to the real site: it dials the camouflage
// address (the mimicked site on 443 by default) and pipes raw bytes both ways. TLS was not
// terminated here, so the stranger completes a genuine handshake with the real site and sees its
// real certificate and content, exactly as if it had connected to that site. If the real site
// cannot be reached the connection is simply closed, as a flaky server would behave; nothing of
// our own is ever shown, so there is no artefact to fingerprint.
func HandleActiveProbe(conn net.Conn, cfg *MimicryConfig) {
	defer conn.Close()

	dest := cfg.FallbackAddr
	if dest == "" && cfg.ServerName != "" {
		dest = net.JoinHostPort(cfg.ServerName, "443")
	}
	if dest == "" {
		return
	}
	upstream, err := net.DialTimeout("tcp", dest, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}
