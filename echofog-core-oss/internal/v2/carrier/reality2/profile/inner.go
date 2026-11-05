package profile

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Secrets are the server traffic secrets of one TLS 1.3 connection, taken from the client's key
// log (the measuring client is ours, so it can write them). They let Observe read the type and
// the exact length of every encrypted handshake message — the Certificate length and the
// number and size of NewSessionTicket messages that wire.md §3.4 lists. They are never part of
// what an observer has; they only name what the record lengths he sees are made of.
type Secrets struct {
	ServerHandshake []byte
	ServerTraffic   []byte
}

// ParseKeyLog reads the NSS key log lines of a single connection.
func ParseKeyLog(b []byte) *Secrets {
	var s Secrets
	for _, line := range bytes.Split(b, []byte("\n")) {
		f := bytes.Fields(line)
		if len(f) != 3 {
			continue
		}
		secret, err := hex.DecodeString(string(f[2]))
		if err != nil {
			continue
		}
		switch string(f[0]) {
		case "SERVER_HANDSHAKE_TRAFFIC_SECRET":
			s.ServerHandshake = secret
		case "SERVER_TRAFFIC_SECRET_0":
			s.ServerTraffic = secret
		}
	}
	return &s
}

const (
	handshakeNewSessionTicket = 4
	handshakeFinished         = 20
)

// ObserveTCPKeyed is ObserveTCP plus the decrypted handshake message lengths (Vector.Inner and
// Vector.Tickets). With nil secrets it is ObserveTCP.
func ObserveTCPKeyed(events []Event, sec *Secrets) (Vector, error) {
	v, srv, err := observeTCP(events)
	if err != nil || sec == nil {
		return v, err
	}
	if err := decryptInner(&v, srv, sec); err != nil {
		return v, err
	}
	return v, nil
}

type aeadKeys struct {
	aead cipher.AEAD
	iv   []byte
	seq  uint64
}

func newKeys(secret []byte, suite uint16) (*aeadKeys, error) {
	var h func() hash.Hash
	var keyLen int
	switch suite {
	case 0x1301:
		h, keyLen = sha256.New, 16
	case 0x1302:
		h, keyLen = sha512.New384, 32
	case 0x1303:
		h, keyLen = sha256.New, 32
	default:
		return nil, fmt.Errorf("profile: cipher suite 0x%04x is not a TLS 1.3 suite", suite)
	}
	key := expandLabelH(h, secret, "key", keyLen)
	iv := expandLabelH(h, secret, "iv", 12)
	var a cipher.AEAD
	var err error
	if suite == 0x1303 {
		a, err = chacha20poly1305.New(key)
	} else {
		var blk cipher.Block
		if blk, err = aes.NewCipher(key); err == nil {
			a, err = cipher.NewGCM(blk)
		}
	}
	if err != nil {
		return nil, err
	}
	return &aeadKeys{aead: a, iv: iv}, nil
}

func expandLabelH(h func() hash.Hash, secret []byte, label string, n int) []byte {
	full := "tls13 " + label
	info := []byte{byte(n >> 8), byte(n), byte(len(full))}
	info = append(info, full...)
	info = append(info, 0)
	out := make([]byte, n)
	_, _ = io.ReadFull(hkdf.Expand(h, secret, info), out)
	return out
}

// open decrypts one record and returns the inner content type and content.
func (k *aeadKeys) open(r tlsRecord) (byte, []byte, error) {
	nonce := append([]byte(nil), k.iv...)
	for i := 0; i < 8; i++ {
		nonce[11-i] ^= byte(k.seq >> (8 * i))
	}
	k.seq++
	hdr := []byte{recApplicationData, 3, 3, byte(len(r.payload) >> 8), byte(len(r.payload))}
	plain, err := k.aead.Open(nil, nonce, r.payload, hdr)
	if err != nil {
		return 0, nil, errors.New("profile: record does not decrypt with the key log secret")
	}
	i := len(plain)
	for i > 0 && plain[i-1] == 0 {
		i--
	}
	if i == 0 {
		return 0, nil, ErrMalformed
	}
	return plain[i-1], plain[:i-1], nil
}

// decryptInner fills v.Inner (handshake messages up to Finished) and v.Tickets.
func decryptInner(v *Vector, srv []tlsRecord, sec *Secrets) error {
	if v.Version != 0x0304 {
		return nil
	}
	if len(sec.ServerHandshake) == 0 {
		return errors.New("profile: key log has no server handshake secret")
	}
	keys, err := newKeys(sec.ServerHandshake, v.Cipher)
	if err != nil {
		return err
	}
	inHandshake := true
	var buf []byte
	for _, r := range srv {
		if r.typ != recApplicationData {
			continue
		}
		typ, body, err := keys.open(r)
		if err != nil {
			return err
		}
		if typ != recHandshake {
			continue
		}
		buf = append(buf, body...)
		for len(buf) >= 4 {
			n := int(buf[1])<<16 | int(buf[2])<<8 | int(buf[3])
			if len(buf) < 4+n {
				break
			}
			mt := buf[0]
			it := Item{Kind: fmt.Sprintf("msg/%d", mt), Len: 4 + n}
			buf = buf[4+n:]
			switch {
			case inHandshake:
				v.Inner = append(v.Inner, it)
				if mt == handshakeFinished {
					inHandshake = false
					if len(sec.ServerTraffic) == 0 {
						return errors.New("profile: key log has no server traffic secret")
					}
					if keys, err = newKeys(sec.ServerTraffic, v.Cipher); err != nil {
						return err
					}
					if len(buf) != 0 {
						return fmt.Errorf("%w: handshake data after Finished", ErrMalformed)
					}
				}
			case mt == handshakeNewSessionTicket:
				v.Tickets = append(v.Tickets, it)
			}
		}
	}
	v.Decrypted = true
	return nil
}
