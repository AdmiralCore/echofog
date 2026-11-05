package profile

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
)

const (
	extSupportedVersions = 43
	extKeyShare          = 51
)

// serverHello is the open part of a ServerHello or HelloRetryRequest.
type serverHello struct {
	HRR          bool
	Version      uint16
	Cipher       uint16
	Group        uint16
	ExtOrder     []uint16
	SessionIDLen int
}

// parseServerHello parses a handshake message (type, 3-byte length, body) of type server_hello.
func parseServerHello(msg []byte) (serverHello, error) {
	var sh serverHello
	if len(msg) < 4 || msg[0] != 2 {
		return sh, fmt.Errorf("%w: not a ServerHello", ErrMalformed)
	}
	n := int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3])
	if len(msg)-4 < n {
		return sh, fmt.Errorf("%w: truncated ServerHello", ErrMalformed)
	}
	b := msg[4 : 4+n]
	if len(b) < 2+32+1 {
		return sh, fmt.Errorf("%w: short ServerHello", ErrMalformed)
	}
	legacy := binary.BigEndian.Uint16(b)
	sh.Version = legacy
	sh.HRR = string(b[2:34]) == string(helloRetryRequestRandom[:])
	sidLen := int(b[34])
	b = b[35:]
	if len(b) < sidLen+2+1+2 {
		return sh, fmt.Errorf("%w: short ServerHello", ErrMalformed)
	}
	sh.SessionIDLen = sidLen
	b = b[sidLen:]
	sh.Cipher = binary.BigEndian.Uint16(b)
	b = b[3:] // cipher, compression
	extLen := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if len(b) != extLen {
		return sh, fmt.Errorf("%w: ServerHello extensions length", ErrMalformed)
	}
	for len(b) > 0 {
		if len(b) < 4 {
			return sh, fmt.Errorf("%w: extension header", ErrMalformed)
		}
		typ := binary.BigEndian.Uint16(b)
		l := int(binary.BigEndian.Uint16(b[2:]))
		if len(b)-4 < l {
			return sh, fmt.Errorf("%w: extension body", ErrMalformed)
		}
		body := b[4 : 4+l]
		b = b[4+l:]
		sh.ExtOrder = append(sh.ExtOrder, typ)
		switch typ {
		case extSupportedVersions:
			if len(body) != 2 {
				return sh, fmt.Errorf("%w: supported_versions", ErrMalformed)
			}
			sh.Version = binary.BigEndian.Uint16(body)
		case extKeyShare:
			if len(body) < 2 {
				return sh, fmt.Errorf("%w: key_share", ErrMalformed)
			}
			sh.Group = binary.BigEndian.Uint16(body)
		}
	}
	return sh, nil
}

// buildServerHello builds a ServerHello handshake message. It is the test and fuzz seed builder.
func buildServerHello(hrr bool, cipher, group uint16, exts []uint16, sidLen int) []byte {
	var body []byte
	body = binary.BigEndian.AppendUint16(body, tls.VersionTLS12)
	if hrr {
		body = append(body, helloRetryRequestRandom[:]...)
	} else {
		body = append(body, make([]byte, 32)...)
	}
	body = append(body, byte(sidLen))
	body = append(body, make([]byte, sidLen)...)
	body = binary.BigEndian.AppendUint16(body, cipher)
	body = append(body, 0)
	var ext []byte
	for _, t := range exts {
		var eb []byte
		switch t {
		case extSupportedVersions:
			eb = binary.BigEndian.AppendUint16(nil, tls.VersionTLS13)
		case extKeyShare:
			eb = binary.BigEndian.AppendUint16(nil, group)
			if !hrr {
				eb = binary.BigEndian.AppendUint16(eb, 32)
				eb = append(eb, make([]byte, 32)...)
			}
		}
		ext = binary.BigEndian.AppendUint16(ext, t)
		ext = binary.BigEndian.AppendUint16(ext, uint16(len(eb)))
		ext = append(ext, eb...)
	}
	body = binary.BigEndian.AppendUint16(body, uint16(len(ext)))
	body = append(body, ext...)
	return append([]byte{2, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
}
