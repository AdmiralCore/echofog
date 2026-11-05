package reality2

import (
	"crypto/ecdh"
	"crypto/sha256"
	"errors"
	"time"
)

// verifyHello is the token check of wire.md §3.3 step 2 (and §4.2 on TCP): the token must open
// under some live masking key for some card address, and must not be a replay.
func verifyHello(masks []maskEntry, addrs []string, replay *Replay, now time.Time, hello *Hello) bool {
	if hello == nil || len(hello.C) != 32 || len(hello.Token) != TokenLen {
		return false
	}
	C, err := ecdh.X25519().NewPublicKey(hello.C)
	if err != nil {
		return false
	}
	aad := HelloAAD(hello.CH0)
	for _, m := range masks {
		z, err := m.sk.ECDH(C)
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			key := TokenKey(z, m.pub, hello.C, addr)
			if key == nil {
				continue
			}
			if _, err := OpenToken(key, aad, hello.Token, now); err != nil {
				continue
			}
			return !replay.SeenOrAdd(m.id, hello.Token)
		}
	}
	return false
}

// Verifier checks the tokens of ClientHellos that did not come through a Gate, that is, the
// reality2-tcp server of wire.md §4.2. It applies the same rules as the QUIC gate: every live
// masking key, every card address, the timestamp window and the shared replay cache.
type Verifier struct {
	masks  []maskEntry
	addrs  []string
	replay *Replay
	now    func() time.Time
}

// NewVerifier builds a Verifier. The arguments mean what they do in GateConfig.
func NewVerifier(masks []MaskKey, addrs []string, replay *Replay, now func() time.Time) (*Verifier, error) {
	if replay == nil {
		return nil, errors.New("reality2: verifier needs a replay cache")
	}
	if len(masks) == 0 || len(masks) > 8 {
		return nil, errors.New("reality2: verifier needs one to eight masking keys")
	}
	if len(addrs) == 0 {
		return nil, errors.New("reality2: verifier needs at least one token address")
	}
	if now == nil {
		now = time.Now
	}
	v := &Verifier{addrs: append([]string(nil), addrs...), replay: replay, now: now}
	for _, m := range masks {
		if m.Private == nil || m.Private.Curve() != ecdh.X25519() {
			return nil, errors.New("reality2: masking key is not X25519")
		}
		pub := m.Private.PublicKey().Bytes()
		v.masks = append(v.masks, maskEntry{sk: m.Private, pub: pub, id: sha256.Sum256(pub)})
	}
	return v, nil
}

// Verify reports whether hello carries a valid, fresh, unseen token. A true result consumes the
// token: the same hello is a replay the next time.
func (v *Verifier) Verify(hello *Hello) bool {
	return verifyHello(v.masks, v.addrs, v.replay, v.now(), hello)
}
