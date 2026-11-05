package handshake

import (
	"bytes"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/crypto/cryptobyte"
)

// helloRetryRequestRandom is the special ServerHello.random that marks a
// HelloRetryRequest (RFC 8446, section 4.1.3).
var helloRetryRequestRandom = []byte{
	0xcf, 0x21, 0xad, 0x74, 0xe5, 0x9a, 0x61, 0x11,
	0xbe, 0x1d, 0x8c, 0x02, 0x1e, 0x65, 0xb8, 0x91,
	0xc2, 0xa2, 0x11, 0x16, 0x7a, 0xbb, 0x8c, 0x5e,
	0x07, 0x9e, 0x09, 0xe2, 0xc8, 0xa8, 0x33, 0x9c,
}

const (
	handshakeTypeServerHello = 2
	extensionKeyShare        = 0x0033
)

// serverHelloKeyShareGroup returns the key exchange group of a complete TLS
// handshake message (its 4-byte header included) when it is a ServerHello that
// carries a key_share entry. A HelloRetryRequest, any other handshake message,
// and malformed input report ok=false.
//
// EchoFog needs the group to decide sealed mode (wire.md §8.5). crypto/tls
// exposes it as ConnectionState.CurveID; this is the equivalent for uTLS's
// QUIC connection, whose ConnectionState keeps the group in an unexported
// field.
func serverHelloKeyShareGroup(msg []byte) (group utls.CurveID, ok bool) {
	if len(msg) < 4 || msg[0] != handshakeTypeServerHello {
		return 0, false
	}
	s := cryptobyte.String(msg[4:])

	var legacyVersion uint16
	var random []byte
	var sessionID cryptobyte.String
	var cipherSuite uint16
	var compression uint8
	var extensions cryptobyte.String
	if !s.ReadUint16(&legacyVersion) ||
		!s.ReadBytes(&random, 32) ||
		bytes.Equal(random, helloRetryRequestRandom) ||
		!s.ReadUint8LengthPrefixed(&sessionID) ||
		!s.ReadUint16(&cipherSuite) ||
		!s.ReadUint8(&compression) ||
		!s.ReadUint16LengthPrefixed(&extensions) ||
		!s.Empty() {
		return 0, false
	}

	for !extensions.Empty() {
		var extensionType uint16
		var data cryptobyte.String
		if !extensions.ReadUint16(&extensionType) ||
			!extensions.ReadUint16LengthPrefixed(&data) {
			return 0, false
		}
		if extensionType != extensionKeyShare {
			continue
		}
		// KeyShareServerHello: the group, then one key_exchange of 1..2^16-1 bytes.
		var g uint16
		if !data.ReadUint16(&g) {
			return 0, false
		}
		return utls.CurveID(g), true
	}
	return 0, false
}
