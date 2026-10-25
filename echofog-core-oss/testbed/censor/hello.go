package censor

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"sort"
	"strings"
)

// clientHello is what a censor reads from a TLS ClientHello.
type clientHello struct {
	SNI        string
	ALPN       []string
	Grease     bool   // GREASE values: browsers send them, Go's crypto/tls and most tools do not
	Ciphers    int    // cipher suites offered, GREASE left out
	Extensions int    // extensions, GREASE left out
	Print      string // a JA4-like fingerprint: protocol, counts, ALPN and a hash of the sorted lists
}

func isGrease(v uint16) bool { return v&0x0f0f == 0x0a0a && v>>8 == v&0xff }

// parseClientHello reads a handshake message (type 1). quic says whether it came in QUIC.
func parseClientHello(h []byte, quic bool) (clientHello, bool) {
	var ch clientHello
	if len(h) < 4 || h[0] != 1 {
		return ch, false
	}
	l := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	if len(h) < 4+l {
		return ch, false // not all of it yet
	}
	b := h[4 : 4+l]
	if len(b) < 2+32+1 {
		return ch, false
	}
	b = b[34:]
	sid := int(b[0])
	if len(b) < 1+sid+2 {
		return ch, false
	}
	b = b[1+sid:]
	cl := int(binary.BigEndian.Uint16(b))
	if len(b) < 2+cl+1 {
		return ch, false
	}
	var ciphers []string
	for i := 0; i+1 < cl; i += 2 {
		v := binary.BigEndian.Uint16(b[2+i:])
		if isGrease(v) {
			ch.Grease = true
			continue
		}
		ciphers = append(ciphers, fmt.Sprintf("%04x", v))
	}
	b = b[2+cl:]
	comp := int(b[0])
	if len(b) < 1+comp+2 {
		return ch, false
	}
	b = b[1+comp:]
	el := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if len(b) < el {
		return ch, false
	}
	b = b[:el]
	var exts []string
	for len(b) >= 4 {
		t, n := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+n {
			break
		}
		data := b[4 : 4+n]
		b = b[4+n:]
		if isGrease(t) {
			ch.Grease = true
			continue
		}
		exts = append(exts, fmt.Sprintf("%04x", t))
		switch t {
		case 0x0000: // server_name
			if len(data) >= 5 && data[2] == 0 {
				nl := int(binary.BigEndian.Uint16(data[3:]))
				if len(data) >= 5+nl {
					ch.SNI = string(data[5 : 5+nl])
				}
			}
		case 0x0010: // ALPN
			if len(data) >= 2 {
				d := data[2:]
				for len(d) > 0 && int(d[0]) < len(d) {
					ch.ALPN = append(ch.ALPN, string(d[1:1+int(d[0])]))
					d = d[1+int(d[0]):]
				}
			}
		}
	}
	ch.Ciphers, ch.Extensions = len(ciphers), len(exts)
	sort.Strings(ciphers)
	sort.Strings(exts)
	sum := sha256.Sum256([]byte(strings.Join(ciphers, ",") + "_" + strings.Join(exts, ",")))
	proto := "t"
	if quic {
		proto = "q"
	}
	alpn := "00"
	if len(ch.ALPN) > 0 && ch.ALPN[0] != "" {
		a := ch.ALPN[0]
		alpn = string(a[0]) + string(a[len(a)-1])
	}
	sni := "i"
	if ch.SNI != "" {
		sni = "d"
	}
	ch.Print = fmt.Sprintf("%s13%s%02d%02d%s_%s", proto, sni, min(ch.Ciphers, 99), min(ch.Extensions, 99), alpn, hex.EncodeToString(sum[:6]))
	return ch, true
}

// tlsHandshake strips TLS record headers from the start of a client's TCP stream and returns the
// handshake bytes; ok is false when the stream does not start as TLS.
func tlsHandshake(stream []byte) ([]byte, bool) {
	var out []byte
	for len(stream) >= 5 && stream[0] == 0x16 && stream[1] == 0x03 && stream[2] <= 0x04 {
		n := int(binary.BigEndian.Uint16(stream[3:5]))
		if len(stream) < 5+n {
			out = append(out, stream[5:]...)
			return out, true
		}
		out = append(out, stream[5:5+n]...)
		stream = stream[5+n:]
	}
	return out, len(out) > 0
}

// fullyEncrypted applies the rule the Great Firewall uses to block "fully encrypted" traffic
// (Wu et al., USENIX Security 2023) to the first data a client sends. A payload is exempt when its
// bits are not near half ones per byte, when it starts with or mostly is printable text, or when
// it starts as TLS or HTTP. What is left looks like nothing but randomness, which is what GFW
// blocks (it applies the rule to a share of such connections, so being flagged means "at risk").
func fullyEncrypted(p []byte) (flagged bool, why string) {
	if len(p) == 0 {
		return false, "empty"
	}
	ones := 0
	printable, run, longest := 0, 0, 0
	for _, c := range p {
		ones += bits.OnesCount8(c)
		if c >= 0x20 && c <= 0x7e {
			printable++
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	avg := float64(ones) / float64(len(p))
	first6 := len(p) >= 6
	for _, c := range p[:min(6, len(p))] {
		if c < 0x20 || c > 0x7e {
			first6 = false
		}
	}
	switch {
	case avg <= 3.4 || avg >= 4.6:
		return false, fmt.Sprintf("bits per byte %.2f", avg)
	case first6:
		return false, "starts with text"
	case printable*2 > len(p):
		return false, "mostly text"
	case longest > 20:
		return false, "a long run of text"
	case len(p) >= 3 && p[0] == 0x16 && p[1] == 0x03 && p[2] <= 0x04:
		return false, "TLS"
	case startsHTTP(p):
		return false, "HTTP"
	}
	return true, fmt.Sprintf("random-looking (bits per byte %.2f)", avg)
}

func startsHTTP(p []byte) bool {
	for _, m := range []string{"GET ", "POST ", "PUT ", "HEAD ", "CONNECT ", "OPTIONS ", "HTTP/"} {
		if strings.HasPrefix(string(p[:min(len(p), 8)]), m) {
			return true
		}
	}
	return false
}
