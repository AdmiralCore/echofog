package selfcover

import (
	"crypto/ecdh"
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
// §4.1): reality2.PrepareTCPHello, the code the reality2-tcp client uses.
func (m *minter) tcpPrepare(uc *utls.UConn) error {
	return reality2.PrepareTCPHello(uc, m.maskPub, m.addr, m.now)
}
