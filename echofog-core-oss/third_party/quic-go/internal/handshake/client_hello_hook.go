package handshake

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"io"
)

// This file is fork patch 4 (ECHOFOG-PATCH.md): the mechanics behind
// Config.ClientHelloHook. The TLS stack consumes ClientHello.random from its random
// source before it hashes the hello, but EchoFog's REALITY token is a function of
// the finished hello (its associated data is CH0, the hello with a zeroed random).
// A single pass cannot satisfy that; these helpers run the hello twice over one
// recorded randomness stream, so the real pass is byte-for-byte the pre-pass except
// for the 32 random bytes the hook returns.

const clientRandomLen = 32

// ch0Rand records a randomness stream while serving zeros for the first 32 bytes —
// the ClientHello random. The pre-pass hello therefore is CH0 by construction.
type ch0Rand struct {
	src      io.Reader
	recorded []byte   // everything served, in order
	reads    [][]byte // every individual Read after the zeroing, for key recovery
	served   int
}

func newCH0Rand(src io.Reader) *ch0Rand {
	if src == nil {
		src = rand.Reader
	}
	return &ch0Rand{src: src}
}

func (r *ch0Rand) Read(p []byte) (int, error) {
	// crypto/internal/rand.CustomReader probes a custom reader with a discarded one-byte
	// read when GODEBUG=cryptocustomrand=1 (randutil.MaybeReadByte). That probe is not part
	// of the hello, and its presence differs between the two passes, so it is served from a
	// separate source and kept out of the recording.
	if len(p) == 1 {
		if _, err := io.ReadFull(rand.Reader, p); err != nil {
			return 0, err
		}
		return 1, nil
	}
	n, err := r.src.Read(p)
	if n > 0 {
		if r.served < clientRandomLen {
			clear(p[:min(n, clientRandomLen-r.served)])
		}
		r.served += n
		buf := append([]byte(nil), p[:n]...)
		r.recorded = append(r.recorded, buf...)
		r.reads = append(r.reads, buf)
	}
	return n, err
}

// tokenFirstRand replays a recorded stream with token in place of its first 32
// bytes, then continues from fallback once the recording is exhausted.
type tokenFirstRand struct {
	token    [32]byte
	recorded []byte
	off      int
	fallback io.Reader
}

func (r *tokenFirstRand) Read(p []byte) (int, error) {
	// See ch0Rand.Read: the one-byte custom-reader probe is not part of the recorded hello.
	if len(p) == 1 {
		if _, err := io.ReadFull(rand.Reader, p); err != nil {
			return 0, err
		}
		return 1, nil
	}
	n := 0
	for n < len(p) && r.off < len(r.recorded) {
		if r.off < clientRandomLen {
			c := copy(p[n:], r.token[r.off:clientRandomLen])
			n += c
			r.off += c
			continue
		}
		c := copy(p[n:], r.recorded[r.off:])
		n += c
		r.off += c
	}
	if n == len(p) {
		return n, nil
	}
	m, err := r.fallback.Read(p[n:])
	return n + m, err
}

// recoverX25519Key finds the private key of the hello's X25519 key share among the
// 32-byte reads of the pre-pass. uTLS generated that key from the randomness stream
// (io.ReadFull of 32 bytes into a private key), so the read whose public key equals
// the share is the key the live handshake will use, not a copy of it.
func recoverX25519Key(ch0 []byte, reads [][]byte) ([]byte, error) {
	C, ok := helloX25519Share(ch0)
	if !ok {
		return nil, errors.New("quic: ClientHello hook: CH0 has no X25519 key share")
	}
	for _, r := range reads {
		if len(r) != 32 {
			continue
		}
		key, err := ecdh.X25519().NewPrivateKey(r)
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare(key.PublicKey().Bytes(), C) == 1 {
			return append([]byte(nil), r...), nil
		}
	}
	return nil, errors.New("quic: ClientHello hook: the X25519 key share is not derived from the randomness stream")
}

// helloX25519Share returns the X25519 public key of a ClientHello handshake message:
// the X25519 entry, or the last 32 bytes of X25519MLKEM768 when there is none.
func helloX25519Share(msg []byte) ([]byte, bool) {
	if len(msg) < 4+2+32 || msg[0] != 1 {
		return nil, false
	}
	end := 4 + (int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3]))
	if end > len(msg) || end < 4+2+32 {
		return nil, false
	}
	msg = msg[:end]
	p := 4 + 2 + 32
	p += 1 + int(msg[p])
	if p+2 > len(msg) {
		return nil, false
	}
	p += 2 + (int(msg[p])<<8 | int(msg[p+1]))
	if p >= len(msg) {
		return nil, false
	}
	p += 1 + int(msg[p])
	if p+2 > len(msg) {
		return nil, false
	}
	extEnd := p + 2 + (int(msg[p])<<8 | int(msg[p+1]))
	if extEnd > len(msg) {
		return nil, false
	}
	p += 2
	var x25519, hybrid []byte
	for p+4 <= extEnd {
		typ := int(msg[p])<<8 | int(msg[p+1])
		l := int(msg[p+2])<<8 | int(msg[p+3])
		p += 4
		if p+l > extEnd {
			return nil, false
		}
		if typ == 51 { // key_share
			body := msg[p : p+l]
			if len(body) < 2 {
				return nil, false
			}
			n := int(body[0])<<8 | int(body[1])
			if n > len(body)-2 {
				return nil, false
			}
			for q := 2; q+4 <= 2+n; {
				group := int(body[q])<<8 | int(body[q+1])
				kl := int(body[q+2])<<8 | int(body[q+3])
				q += 4
				if q+kl > 2+n {
					return nil, false
				}
				switch group {
				case 0x001d:
					if kl != 32 {
						return nil, false
					}
					x25519 = append([]byte(nil), body[q:q+kl]...)
				case 0x11ec:
					if kl < 32 {
						return nil, false
					}
					hybrid = append([]byte(nil), body[q+kl-32:q+kl]...)
				}
				q += kl
			}
		}
		p += l
	}
	if x25519 != nil {
		return x25519, true
	}
	if hybrid != nil {
		return hybrid, true
	}
	return nil, false
}
