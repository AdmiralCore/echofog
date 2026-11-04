package noise

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Token is a Noise message token.
type Token uint8

// Tokens of the HFS patterns (Noise §7, Noise HFS).
const (
	TokE Token = iota + 1
	TokS
	TokEE
	TokES
	TokSE
	TokE1
	TokEKEM1
)

func (t Token) String() string {
	switch t {
	case TokE:
		return "e"
	case TokS:
		return "s"
	case TokEE:
		return "ee"
	case TokES:
		return "es"
	case TokSE:
		return "se"
	case TokE1:
		return "e1"
	case TokEKEM1:
		return "ekem1"
	}
	return fmt.Sprintf("token(%d)", uint8(t))
}

// Pattern is a handshake pattern. Messages alternate, starting with the initiator.
type Pattern struct {
	Name string
	// ProtocolName is the full Noise protocol name (with the EchoFog/2/ prefix).
	ProtocolName string
	// PreResponderStatic is the pre-message "<- s".
	PreResponderStatic bool
	Messages           [][]Token
}

// The two v2 patterns (handshake.md §4.1, §4.2).
var (
	XKhfs = &Pattern{
		Name:               "XKhfs",
		ProtocolName:       NameXKhfs,
		PreResponderStatic: true,
		Messages: [][]Token{
			{TokE, TokE1, TokES},
			{TokE, TokEE, TokEKEM1},
			{TokS, TokSE},
		},
	}
	XXhfs = &Pattern{
		Name:         "XXhfs",
		ProtocolName: NameXXhfs,
		Messages: [][]Token{
			{TokE, TokE1},
			{TokE, TokEE, TokEKEM1, TokS, TokES},
			{TokS, TokSE},
		},
	}
)

// Sizes of the fixed parts (handshake.md §2.1, FIPS 203).
const (
	DHLen    = 32
	KEMPKLen = mlkem.EncapsulationKeySize768 // 1184
	KEMCTLen = mlkem.CiphertextSize768       // 1088
	KEMSeed  = mlkem.SeedSize                // 64
)

var (
	// ErrState is returned for an operation out of turn or after a failure.
	ErrState = errors.New("noise: operation not allowed in this state")
	// ErrShort is returned when a message is shorter than its tokens.
	ErrShort = errors.New("noise: message too short")
	// ErrTooLong is returned when a message exceeds MaxMessageLen.
	ErrTooLong = errors.New("noise: message too long")
	// ErrDH is returned for an invalid public key or an all-zero X25519 output.
	ErrDH = errors.New("noise: invalid DH")
	// ErrBadKEMKey is returned when an ML-KEM-768 encapsulation key fails the FIPS 203 check.
	ErrBadKEMKey = errors.New("noise: invalid ML-KEM-768 encapsulation key")
	// ErrBadKEMCiphertext is returned when decapsulation rejects the ciphertext format.
	ErrBadKEMCiphertext = errors.New("noise: invalid ML-KEM-768 ciphertext")
)

// TraceEvent reports the state after a step, for transcript vectors.
type TraceEvent struct {
	Message int    // 1-based message number; 0 for initialisation
	Token   string // "init", "prologue", "pre:s", a token name, or "payload"
	H, CK   []byte
	K       []byte // current cipher key, nil if none
	N       uint64 // next nonce
}

// Config configures one side of a handshake.
type Config struct {
	Pattern   *Pattern
	Initiator bool
	Prologue  []byte
	// Static is the local static X25519 key (s). Required for both patterns.
	Static *ecdh.PrivateKey
	// RemoteStatic is rs, the responder's static public key; required for an XK initiator.
	RemoteStatic []byte

	// Rand is the entropy source for ephemerals; nil means crypto/rand.
	Rand io.Reader

	// Ephemeral, KEMSeed and KEMEncapsulate fix the handshake's randomness. They exist for test
	// vectors only; production code leaves them nil.
	Ephemeral      *ecdh.PrivateKey
	KEMSeed        []byte
	KEMEncapsulate func(ek *mlkem.EncapsulationKey768) (sharedKey, ciphertext []byte, err error)

	// Trace, if set, is called after every step.
	Trace func(TraceEvent)
}

// HandshakeState is the Noise HandshakeState with the HFS tokens.
type HandshakeState struct {
	ss        *SymmetricState
	pattern   *Pattern
	initiator bool
	s         *ecdh.PrivateKey
	e         *ecdh.PrivateKey
	rs, re    []byte
	e1        *mlkem.DecapsulationKey768
	re1       *mlkem.EncapsulationKey768
	msg       int // index of the next message
	failed    bool
	hPayload  []byte
	cfg       Config
}

// NewHandshake runs Initialize (Noise §5.3): InitializeSymmetric, MixHash(prologue), pre-messages.
func NewHandshake(cfg Config) (*HandshakeState, error) {
	if cfg.Pattern == nil {
		return nil, errors.New("noise: no pattern")
	}
	if cfg.Static == nil || cfg.Static.Curve() != ecdh.X25519() {
		return nil, errors.New("noise: static X25519 key required")
	}
	hs := &HandshakeState{
		ss:        NewSymmetricState(cfg.Pattern.ProtocolName),
		pattern:   cfg.Pattern,
		initiator: cfg.Initiator,
		s:         cfg.Static,
		cfg:       cfg,
	}
	hs.trace(0, "init")
	hs.ss.MixHash(cfg.Prologue)
	hs.trace(0, "prologue")
	if cfg.Pattern.PreResponderStatic {
		if cfg.Initiator {
			if len(cfg.RemoteStatic) != DHLen {
				return nil, errors.New("noise: XK initiator needs the responder static key")
			}
			hs.rs = append([]byte(nil), cfg.RemoteStatic...)
			hs.ss.MixHash(hs.rs)
		} else {
			hs.ss.MixHash(hs.s.PublicKey().Bytes())
		}
		hs.trace(0, "pre:s")
	}
	return hs, nil
}

func (hs *HandshakeState) trace(msg int, tok string) {
	if hs.cfg.Trace == nil {
		return
	}
	cs := hs.ss.Cipher()
	hs.cfg.Trace(TraceEvent{Message: msg, Token: tok, H: hs.ss.H(), CK: hs.ss.CK(), K: cs.Key(), N: cs.Nonce()})
}

func (hs *HandshakeState) rand() io.Reader {
	if hs.cfg.Rand != nil {
		return hs.cfg.Rand
	}
	return rand.Reader
}

// myTurn reports whether the local side writes the next message.
func (hs *HandshakeState) myTurn() bool { return (hs.msg%2 == 0) == hs.initiator }

// Complete reports whether all messages were processed.
func (hs *HandshakeState) Complete() bool { return !hs.failed && hs.msg == len(hs.pattern.Messages) }

// MessageIndex returns the 1-based number of the next message.
func (hs *HandshakeState) MessageIndex() int { return hs.msg + 1 }

// H returns the current handshake hash.
func (hs *HandshakeState) H() []byte { return hs.ss.H() }

// CK returns the current chaining key.
func (hs *HandshakeState) CK() []byte { return hs.ss.CK() }

// HashBeforePayload returns h as it was immediately before the payload of the last message
// written or read (handshake.md §5: h1 for the access proof).
func (hs *HandshakeState) HashBeforePayload() []byte { return append([]byte(nil), hs.hPayload...) }

// RemoteStatic returns rs (nil until known).
func (hs *HandshakeState) RemoteStatic() []byte { return append([]byte(nil), hs.rs...) }

// RemoteEphemeral returns re (nil until known).
func (hs *HandshakeState) RemoteEphemeral() []byte { return append([]byte(nil), hs.re...) }

// LocalEphemeral returns e.pub (nil until generated).
func (hs *HandshakeState) LocalEphemeral() []byte {
	if hs.e == nil {
		return nil
	}
	return hs.e.PublicKey().Bytes()
}

// LocalStatic returns s.pub.
func (hs *HandshakeState) LocalStatic() []byte { return hs.s.PublicKey().Bytes() }

// HasKey reports whether the symmetric state has a key (payloads are encrypted).
func (hs *HandshakeState) HasKey() bool { return hs.ss.Cipher().HasKey() }

// generateX25519 is GENERATE_KEYPAIR(): 32 bytes from r. (crypto/ecdh's GenerateKey ignores its
// reader since Go 1.26; reading here keeps Config.Rand meaningful for tests.)
func generateX25519(r io.Reader) (*ecdh.PrivateKey, error) {
	var b [32]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return nil, err
	}
	k, err := ecdh.X25519().NewPrivateKey(b[:])
	wipe(b[:])
	return k, err
}

func dh(k *ecdh.PrivateKey, pub []byte) ([]byte, error) {
	p, err := ecdh.X25519().NewPublicKey(pub)
	if err != nil {
		return nil, ErrDH
	}
	z, err := k.ECDH(p) // returns an error for an all-zero output
	if err != nil {
		return nil, ErrDH
	}
	return z, nil
}

func (hs *HandshakeState) mixDH(k *ecdh.PrivateKey, pub []byte) error {
	z, err := dh(k, pub)
	if err != nil {
		return err
	}
	hs.ss.MixKey(z)
	wipe(z)
	return nil
}

// WriteMessage appends the next message with payload to out.
func (hs *HandshakeState) WriteMessage(out, payload []byte) ([]byte, error) {
	return hs.WriteMessageFunc(out, func([]byte) ([]byte, error) { return payload, nil })
}

// WriteMessageFunc is WriteMessage with a payload computed from h after the message's tokens.
func (hs *HandshakeState) WriteMessageFunc(out []byte, payload func(h []byte) ([]byte, error)) (res []byte, err error) {
	if hs.failed || hs.msg >= len(hs.pattern.Messages) || !hs.myTurn() {
		return nil, ErrState
	}
	defer func() {
		if err != nil {
			hs.fail()
		}
	}()
	num := hs.msg + 1
	start := len(out)
	for _, tok := range hs.pattern.Messages[hs.msg] {
		switch tok {
		case TokE:
			if hs.e == nil {
				if hs.cfg.Ephemeral != nil {
					hs.e = hs.cfg.Ephemeral
				} else if hs.e, err = generateX25519(hs.rand()); err != nil {
					return nil, err
				}
			}
			pub := hs.e.PublicKey().Bytes()
			out = append(out, pub...)
			hs.ss.MixHash(pub)
		case TokE1:
			seed := hs.cfg.KEMSeed
			if seed == nil {
				seed = make([]byte, KEMSeed)
				if _, err = io.ReadFull(hs.rand(), seed); err != nil {
					return nil, err
				}
			}
			if hs.e1, err = mlkem.NewDecapsulationKey768(seed); err != nil {
				return nil, err
			}
			if hs.cfg.KEMSeed == nil {
				wipe(seed)
			}
			if out, err = hs.ss.EncryptAndHash(out, hs.e1.EncapsulationKey().Bytes()); err != nil {
				return nil, err
			}
		case TokEKEM1:
			if hs.re1 == nil {
				return nil, ErrState
			}
			var ss, ct []byte
			if hs.cfg.KEMEncapsulate != nil {
				ss, ct, err = hs.cfg.KEMEncapsulate(hs.re1)
				if err != nil {
					return nil, err
				}
			} else {
				ss, ct = hs.re1.Encapsulate()
			}
			if out, err = hs.ss.EncryptAndHash(out, ct); err != nil {
				return nil, err
			}
			hs.ss.MixKey(ss)
			wipe(ss)
			hs.re1 = nil
		case TokS:
			if out, err = hs.ss.EncryptAndHash(out, hs.s.PublicKey().Bytes()); err != nil {
				return nil, err
			}
		case TokEE:
			err = hs.mixDH(hs.e, hs.re)
		case TokES:
			if hs.initiator {
				err = hs.mixDH(hs.e, hs.rs)
			} else {
				err = hs.mixDH(hs.s, hs.re)
			}
		case TokSE:
			if hs.initiator {
				err = hs.mixDH(hs.s, hs.re)
			} else {
				err = hs.mixDH(hs.e, hs.rs)
			}
		default:
			return nil, fmt.Errorf("noise: unknown token %v", tok)
		}
		if err != nil {
			return nil, err
		}
		hs.trace(num, tok.String())
	}
	hs.hPayload = hs.ss.H()
	p, err := payload(hs.ss.H())
	if err != nil {
		return nil, err
	}
	if out, err = hs.ss.EncryptAndHash(out, p); err != nil {
		return nil, err
	}
	hs.trace(num, "payload")
	if len(out)-start > MaxMessageLen {
		return nil, ErrTooLong
	}
	hs.msg++
	return out, nil
}

func (hs *HandshakeState) take(msg *[]byte, n int) ([]byte, error) {
	if hs.ss.Cipher().HasKey() {
		n += TagLen
	}
	if len(*msg) < n {
		return nil, ErrShort
	}
	b := (*msg)[:n]
	*msg = (*msg)[n:]
	return b, nil
}

// ReadMessage processes the next message and returns its payload.
func (hs *HandshakeState) ReadMessage(msg []byte) (payload []byte, err error) {
	if hs.failed || hs.msg >= len(hs.pattern.Messages) || hs.myTurn() {
		return nil, ErrState
	}
	if len(msg) > MaxMessageLen {
		hs.fail()
		return nil, ErrTooLong
	}
	defer func() {
		if err != nil {
			hs.fail()
		}
	}()
	num := hs.msg + 1
	for _, tok := range hs.pattern.Messages[hs.msg] {
		switch tok {
		case TokE:
			if len(msg) < DHLen {
				return nil, ErrShort
			}
			hs.re = append([]byte(nil), msg[:DHLen]...)
			msg = msg[DHLen:]
			hs.ss.MixHash(hs.re)
		case TokE1:
			var c, pk []byte
			if c, err = hs.take(&msg, KEMPKLen); err != nil {
				return nil, err
			}
			if pk, err = hs.ss.DecryptAndHash(c); err != nil {
				return nil, err
			}
			if hs.re1, err = mlkem.NewEncapsulationKey768(pk); err != nil {
				return nil, ErrBadKEMKey
			}
		case TokEKEM1:
			if hs.e1 == nil {
				return nil, ErrState
			}
			var c, ct, ss []byte
			if c, err = hs.take(&msg, KEMCTLen); err != nil {
				return nil, err
			}
			if ct, err = hs.ss.DecryptAndHash(c); err != nil {
				return nil, err
			}
			if ss, err = hs.e1.Decapsulate(ct); err != nil {
				return nil, ErrBadKEMCiphertext
			}
			hs.ss.MixKey(ss)
			wipe(ss)
			hs.e1 = nil // the KEM key pair is erased after ekem1 (handshake.md §2.1)
		case TokS:
			var c, pub []byte
			if c, err = hs.take(&msg, DHLen); err != nil {
				return nil, err
			}
			if pub, err = hs.ss.DecryptAndHash(c); err != nil {
				return nil, err
			}
			hs.rs = pub
		case TokEE:
			err = hs.mixDH(hs.e, hs.re)
		case TokES:
			if hs.initiator {
				err = hs.mixDH(hs.e, hs.rs)
			} else {
				err = hs.mixDH(hs.s, hs.re)
			}
		case TokSE:
			if hs.initiator {
				err = hs.mixDH(hs.s, hs.re)
			} else {
				err = hs.mixDH(hs.e, hs.rs)
			}
		default:
			return nil, fmt.Errorf("noise: unknown token %v", tok)
		}
		if err != nil {
			return nil, err
		}
		hs.trace(num, tok.String())
	}
	hs.hPayload = hs.ss.H()
	if hs.ss.Cipher().HasKey() && len(msg) < TagLen {
		return nil, ErrShort
	}
	if payload, err = hs.ss.DecryptAndHash(msg); err != nil {
		return nil, err
	}
	hs.trace(num, "payload")
	hs.msg++
	return payload, nil
}

// Split returns the transport cipher states (k_i2r, k_r2i) after the last message.
func (hs *HandshakeState) Split() (i2r, r2i *CipherState, err error) {
	if !hs.Complete() {
		return nil, nil, ErrState
	}
	i2r, r2i = hs.ss.Split()
	return i2r, r2i, nil
}

func (hs *HandshakeState) fail() {
	hs.failed = true
	hs.e1 = nil
	hs.re1 = nil
}

// Erase drops the ephemeral keys and clears the symmetric state (handshake.md §9). The X25519
// private key types of crypto/ecdh cannot be cleared in place; the references are dropped.
func (hs *HandshakeState) Erase() {
	hs.ss.Erase()
	hs.e = nil
	hs.e1 = nil
	hs.re1 = nil
	hs.failed = true
}
