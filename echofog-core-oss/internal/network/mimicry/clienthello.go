package mimicry

import (
	"crypto/sha256"
	"io"
	"net"
)

// The first bytes a TLS client sends are a handshake record carrying the ClientHello. REALITY
// hides the authorization token in the ClientHello's legacy_session_id: in TLS 1.3 that field is
// 32 random bytes sent only for compatibility (RFC 8446 4.1.2), so a token there is
// indistinguishable from an ordinary session id. The server reads the record before it answers,
// takes the token out, and decides whether the caller is one of ours or a stranger to pass on to
// the real site — without ever terminating the stranger's TLS itself.

const (
	// maxClientHelloRecord bounds the first record we buffer (a real ClientHello is well under this).
	maxClientHelloRecord = 16 * 1024
	// tlsSessionIDLen is the session id a TLS 1.3 client sends in compatibility mode.
	tlsSessionIDLen = 32
)

// readClientHelloRecord reads the first TLS handshake record from conn. It returns everything it
// read (so those bytes can be replayed to our own TLS server or to the real site untouched) and
// whether a complete, well-formed ClientHello record was obtained. On a short or malformed read
// the bytes read so far are still returned, so a stranger's junk is relayed on, never dropped in a
// way that would single our port out.
func readClientHelloRecord(conn net.Conn) (buf []byte, ok bool) {
	hdr := make([]byte, 5)
	n, err := io.ReadFull(conn, hdr)
	if err != nil {
		return hdr[:n], false
	}
	// content type 22 = handshake; 0x03xx legacy record version
	if hdr[0] != 22 || hdr[1] != 3 {
		return hdr, false
	}
	length := int(hdr[3])<<8 | int(hdr[4])
	if length == 0 || length > maxClientHelloRecord {
		return hdr, false
	}
	rec := make([]byte, 5+length)
	copy(rec, hdr)
	m, err := io.ReadFull(conn, rec[5:])
	if err != nil {
		return rec[:5+m], false
	}
	return rec, true
}

// extKeyShare is the TLS 1.3 key_share extension (RFC 8446 4.2.8).
const extKeyShare = 51

// helloBinding is what a client's token is sealed to: a hash of the ClientHello's random and its
// key_share extension. msg is the handshake message (type byte first, no record header). The
// session id is not part of it, so the client can compute it before the token is in place.
func helloBinding(msg []byte) ([]byte, bool) {
	const randomOff = 4 + 2 // handshake header, client_version
	const idLenOff = randomOff + 32
	if len(msg) < idLenOff+1 || msg[0] != 1 {
		return nil, false
	}
	if n := int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3]); 4+n > len(msg) {
		return nil, false // the hello goes on in another record: not one of ours
	} else {
		msg = msg[:4+n]
	}
	random := msg[randomOff:idLenOff]
	p := idLenOff + 1 + int(msg[idLenOff]) // past the session id
	if p+2 > len(msg) {
		return nil, false
	}
	p += 2 + (int(msg[p])<<8 | int(msg[p+1])) // cipher suites
	if p+1 > len(msg) {
		return nil, false
	}
	p += 1 + int(msg[p]) // compression methods
	if p+2 > len(msg) {
		return nil, false
	}
	end := p + 2 + (int(msg[p])<<8 | int(msg[p+1]))
	if end > len(msg) {
		return nil, false
	}
	for p += 2; p+4 <= end; {
		typ := int(msg[p])<<8 | int(msg[p+1])
		l := int(msg[p+2])<<8 | int(msg[p+3])
		p += 4
		if p+l > end {
			return nil, false
		}
		if typ == extKeyShare {
			h := sha256.New()
			h.Write([]byte("echofog-hello-binding-v1"))
			h.Write(random)
			h.Write(msg[p : p+l])
			return h.Sum(nil), true
		}
		p += l
	}
	return nil, false
}

// sessionIDFromClientHello extracts the legacy_session_id from a ClientHello record. The layout is
// fixed up to that field, so no full TLS parser is needed:
//
//	record header 5 | handshake type 1 | handshake len 3 | client_version 2 | random 32 |
//	session_id_len 1 | session_id ...
func sessionIDFromClientHello(rec []byte) ([]byte, bool) {
	const idLenOff = 5 + 4 + 2 + 32
	if len(rec) < idLenOff+1 {
		return nil, false
	}
	if rec[5] != 1 { // handshake type 1 = ClientHello
		return nil, false
	}
	idLen := int(rec[idLenOff])
	start := idLenOff + 1
	if idLen == 0 || start+idLen > len(rec) {
		return nil, false
	}
	return rec[start : start+idLen], true
}
