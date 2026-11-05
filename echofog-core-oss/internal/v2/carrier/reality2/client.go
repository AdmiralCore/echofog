package reality2

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	quic "github.com/apernet/quic-go"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/quicx"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// ErrRandomNotHonoured reports a Go toolchain that ignores tls.Config.Rand for X25519.
// Since Go 1.24 crypto/ecdh uses the process CSPRNG unless GODEBUG=cryptocustomrand=1 is set,
// and without it the client cannot know the private key of its own key share, which the token
// of wire.md §3.2 is derived from.
var ErrRandomNotHonoured = errors.New("reality2: the Go toolchain ignores tls.Config.Rand for X25519; set GODEBUG=cryptocustomrand=1")

// ErrCard reports a card or address that cannot be dialed with this suite.
var ErrCard = errors.New("reality2: unusable card or address")

// DialerConfig configures a Dialer.
type DialerConfig struct {
	// Group is the key exchange group the cover selected, from the cover profile (T2.6b).
	// Zero leaves the TLS preference at the library default.
	Group tls.CurveID
	// Now is the injectable clock for token timestamps; nil means time.Now.
	Now func() time.Time
}

// Dialer dials one reality2-quic address of a verified card (wire.md §3.1–3.2).
type Dialer struct {
	cfg DialerConfig

	randOnce sync.Once
	randOK   bool
}

// NewDialer builds the suite's dialer.
func NewDialer(cfg DialerConfig) *Dialer { return &Dialer{cfg: cfg} }

var _ carrier.Dialer = (*Dialer)(nil)

// Dial opens one QUIC connection whose ClientHello carries a REALITY v2 token.
func (d *Dialer) Dial(ctx context.Context, req carrier.DialRequest) (mux.Carrier, error) {
	a := req.Addr
	if a == nil || a.GetSuite() != objects.SuiteReality2QUIC {
		return nil, fmt.Errorf("%w: address is not reality2-quic", ErrCard)
	}
	if req.Card == nil {
		return nil, fmt.Errorf("%w: no card", ErrCard)
	}
	maskPub, err := maskPublic(req.Card, a.GetMask())
	if err != nil {
		return nil, err
	}
	sni := objects.SuiteParams(a).GetSni()
	if sni == "" {
		return nil, fmt.Errorf("%w: no SNI in the card", ErrCard)
	}
	addr := AddressString(a.GetHost(), a.GetPort())
	if a.GetHost() == "" || a.GetPort() == 0 {
		return nil, fmt.Errorf("%w: no host or port", ErrCard)
	}
	if !d.customRandHonoured() {
		return nil, ErrRandomNotHonoured
	}

	clientTLS, _ := quicx.Reality2TLSConfig(d.cfg.Group, nil)
	clientTLS.ServerName = sni
	now := d.cfg.Now
	if now == nil {
		now = time.Now
	}
	maskKey, err := ecdh.X25519().NewPublicKey(maskPub)
	if err != nil {
		return nil, fmt.Errorf("%w: masking key: %v", ErrCard, err)
	}

	conf := quicx.Reality2QUICConfig()
	conf.ClientHelloHook = func(ch0, c []byte) ([32]byte, error) {
		priv, err := ecdh.X25519().NewPrivateKey(c)
		if err != nil {
			return [32]byte{}, fmt.Errorf("reality2: ClientHello key share: %w", err)
		}
		z, err := priv.ECDH(maskKey)
		if err != nil {
			return [32]byte{}, fmt.Errorf("reality2: token key: %w", err)
		}
		token, err := SealToken(TokenKey(z, maskPub, priv.PublicKey().Bytes(), addr), HelloAAD(ch0), uint32(now().Unix()))
		if err != nil {
			return [32]byte{}, err
		}
		var out [32]byte
		copy(out[:], token)
		return out, nil
	}

	conn, err := quic.DialAddr(ctx, addr, clientTLS, conf)
	if err != nil {
		return nil, fmt.Errorf("reality2: dial %s: %w", addr, err)
	}
	return quicx.Wrap(conn, true), nil
}

// maskPublic returns the masking public key a card address refers to (Addr.mask is 1-based).
func maskPublic(card *objects.Card, mask uint32) ([]byte, error) {
	masks := card.MaskKeys()
	if mask == 0 || int(mask) > len(masks) {
		return nil, fmt.Errorf("%w: mask index %d out of range", ErrCard, mask)
	}
	key, err := keys.ParseKind(masks[mask-1].GetKey(), keys.X25519)
	if err != nil {
		return nil, fmt.Errorf("%w: masking key: %v", ErrCard, err)
	}
	return key.Key(), nil
}

// customRandHonoured probes the toolchain once. See ErrRandomNotHonoured.
func (d *Dialer) customRandHonoured() bool {
	d.randOnce.Do(func() {
		seed := bytes.Repeat([]byte{0x5a}, 32)
		k, err := ecdh.X25519().GenerateKey(bytes.NewReader(seed))
		if err != nil {
			return
		}
		want, err := ecdh.X25519().NewPrivateKey(seed)
		if err != nil {
			return
		}
		d.randOK = bytes.Equal(k.PublicKey().Bytes(), want.PublicKey().Bytes())
	})
	return d.randOK
}
