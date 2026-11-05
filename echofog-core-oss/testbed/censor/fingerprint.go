package censor

import (
	"crypto/sha256"
	"encoding/hex"
)

// Fingerprint is the part of a ClientHello a profile database and the D4 detector need.
type Fingerprint struct {
	JA3        string   `json:"ja3"`
	JA4        string   `json:"ja4"`
	SNI        string   `json:"sni,omitempty"`
	ALPN       []string `json:"alpn,omitempty"`
	Grease     bool     `json:"grease"`
	Ciphers    int      `json:"ciphers"`
	Extensions int      `json:"extensions"`
	Version    uint16   `json:"version"`
	PQ         bool     `json:"pq"`
	ECH        bool     `json:"ech"`
	Bytes      int      `json:"bytes"`
	SHA256     string   `json:"sha256"`
}

func (c clientHello) fingerprint(raw []byte) Fingerprint {
	sum := sha256.Sum256(raw)
	return Fingerprint{
		JA3: c.JA3, JA4: c.JA4, SNI: c.SNI, ALPN: c.ALPN, Grease: c.Grease,
		Ciphers: c.Ciphers, Extensions: c.Extensions, Version: c.Version,
		PQ: c.PQ, ECH: c.ECH, Bytes: len(raw), SHA256: hex.EncodeToString(sum[:8]),
	}
}

// ParseTLSClientHello reads the first TLS records of a TCP stream and returns the ClientHello
// fingerprint (JA3, JA4-like, GREASE, PQ, ECH, ALPN).
func ParseTLSClientHello(stream []byte) (Fingerprint, bool) {
	hs, ok := tlsHandshake(stream)
	if !ok {
		return Fingerprint{}, false
	}
	ch, ok := parseClientHello(hs, false)
	if !ok {
		return Fingerprint{}, false
	}
	return ch.fingerprint(hs), true
}

// QUICFingerprint is what the first datagram says about a QUIC client.
type QUICFingerprint struct {
	Version   uint32   `json:"version"`
	Long      bool     `json:"long"`
	FixedBit  bool     `json:"fixed_bit"`
	FirstSize int      `json:"first_size"`
	Decrypted bool     `json:"decrypted"`
	JA4       string   `json:"ja4,omitempty"`
	SNI       string   `json:"sni,omitempty"`
	ALPN      []string `json:"alpn,omitempty"`
}

// ParseQUICDatagram opens the Initials in one UDP datagram, the way a censor does.
func ParseQUICDatagram(dgram []byte) QUICFingerprint {
	q := openInitials(dgram)
	res := QUICFingerprint{Version: q.version, Long: q.isLongHeader, Decrypted: q.decrypted, FirstSize: len(dgram)}
	if len(dgram) > 0 {
		res.FixedBit = dgram[0]&0x40 != 0
	}
	if q.decrypted {
		if ch, ok := parseClientHello(assembleCrypto(q.crypto), true); ok {
			res.JA4, res.SNI, res.ALPN = ch.JA4, ch.SNI, ch.ALPN
		}
	}
	return res
}

// ParseQUICClientHello assembles the Initials of several datagrams and parses the QUIC ClientHello.
func ParseQUICClientHello(datagrams ...[]byte) (Fingerprint, bool) {
	crypto := map[int][]byte{}
	for _, d := range datagrams {
		q := openInitials(d)
		for o, part := range q.crypto {
			crypto[o] = part
		}
	}
	if len(crypto) == 0 {
		return Fingerprint{}, false
	}
	raw := assembleCrypto(crypto)
	ch, ok := parseClientHello(raw, true)
	if !ok {
		return Fingerprint{}, false
	}
	return ch.fingerprint(raw), true
}
