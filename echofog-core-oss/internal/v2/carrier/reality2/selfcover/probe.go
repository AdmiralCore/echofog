package selfcover

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"fmt"
	"time"

	quic "github.com/apernet/quic-go"
	utls "github.com/refraction-networking/utls"

	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
)

// minter makes the tokens of the node's own check connections. The own check is a client of the
// node's first masking key at its first card address: it sees exactly what a client sees.
type minter struct {
	maskPub []byte
	addr    string
	now     func() time.Time
}

// seal returns the token for a hello whose CH0 is ch0 and whose X25519 key share has private
// key c.
func (m *minter) seal(ch0 []byte, c *ecdh.PrivateKey) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(m.maskPub)
	if err != nil {
		return nil, err
	}
	z, err := c.ECDH(pub)
	if err != nil {
		return nil, err
	}
	return reality2.SealToken(reality2.TokenKey(z, m.maskPub, c.PublicKey().Bytes(), m.addr),
		reality2.HelloAAD(ch0), uint32(m.now().Unix()))
}

// quicHook is the ClientHelloHook of the fork (patch 4) for a token-carrying QUIC hello.
func (m *minter) quicHook(conf *quic.Config) {
	conf.ClientHelloHook = func(ch0, c []byte) ([32]byte, error) {
		priv, err := ecdh.X25519().NewPrivateKey(c)
		if err != nil {
			return [32]byte{}, err
		}
		tok, err := m.seal(ch0, priv)
		if err != nil {
			return [32]byte{}, err
		}
		var out [32]byte
		copy(out[:], tok)
		return out, nil
	}
}

// tcpPrepare puts a token into the 32-byte session id of a Chrome-shaped uTLS hello (wire.md
// §4.1): the hello is built with a zero session id, CH0 is taken from it, and the token replaces
// the zeros. The second marshal must give the same CH0, or the token would not bind.
func (m *minter) tcpPrepare(uc *utls.UConn) error {
	if err := uc.BuildHandshakeState(); err != nil {
		return err
	}
	hello := uc.HandshakeState.Hello
	hello.SessionId = make([]byte, 32)
	if err := uc.MarshalClientHello(); err != nil {
		return err
	}
	parsed, err := reality2.ParseHelloTCP(hello.Raw)
	if err != nil {
		return fmt.Errorf("own hello: %w", err)
	}
	ks := uc.HandshakeState.State13.KeyShareKeys
	if ks == nil || ks.Ecdhe == nil || !bytes.Equal(ks.Ecdhe.PublicKey().Bytes(), parsed.C) {
		return errors.New("selfcover: the hello's X25519 share has no private key to bind the token to")
	}
	tok, err := m.seal(parsed.CH0, ks.Ecdhe)
	if err != nil {
		return err
	}
	hello.SessionId = tok
	if err := uc.MarshalClientHello(); err != nil {
		return err
	}
	again, err := reality2.ParseHelloTCP(hello.Raw)
	if err != nil || !bytes.Equal(again.CH0, parsed.CH0) {
		return errors.New("selfcover: the hello changed between the two marshals")
	}
	return nil
}
