package censor

import (
	"crypto/md5" // #nosec G501 -- JA3 is defined as an MD5, it is a label, not a security primitive
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
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
	Print      string // the JA4 fingerprint (kept for the old reports)

	Version           uint16 // legacy_version, normally 0x0303
	SupportedVersions []uint16
	CipherList        []uint16 // in the order offered, GREASE left out
	ExtList           []uint16 // in the order offered, GREASE left out
	Groups            []uint16 // supported groups, GREASE left out
	PointFormats      []byte
	SigAlgs           []uint16
	PQ                bool // a post-quantum key share (X25519MLKEM768 or Kyber768)
	ECH               bool // an encrypted-client-hello extension (GREASE-ECH included)
	JA3               string
	JA4               string
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
	ch.Version = binary.BigEndian.Uint16(b[:2])
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
		ch.CipherList = append(ch.CipherList, v)
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
		ch.ExtList = append(ch.ExtList, t)
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
		case 0x000a: // supported_groups
			if len(data) >= 2 {
				n := int(binary.BigEndian.Uint16(data))
				d := data[2:]
				for i := 0; i+1 < n && i+1 < len(d); i += 2 {
					v := binary.BigEndian.Uint16(d[i:])
					if isGrease(v) {
						ch.Grease = true
						continue
					}
					ch.Groups = append(ch.Groups, v)
				}
			}
		case 0x000b: // ec_point_formats
			if len(data) >= 1 {
				n := int(data[0])
				if n > len(data)-1 {
					n = len(data) - 1
				}
				ch.PointFormats = append(ch.PointFormats, data[1:1+n]...)
			}
		case 0x000d: // signature_algorithms
			if len(data) >= 2 {
				n := int(binary.BigEndian.Uint16(data))
				d := data[2:]
				for i := 0; i+1 < n && i+1 < len(d); i += 2 {
					v := binary.BigEndian.Uint16(d[i:])
					if isGrease(v) {
						ch.Grease = true
						continue
					}
					ch.SigAlgs = append(ch.SigAlgs, v)
				}
			}
		case 0x002b: // supported_versions
			if len(data) >= 1 {
				n := int(data[0])
				d := data[1:]
				for i := 0; i+1 < n && i+1 < len(d); i += 2 {
					v := binary.BigEndian.Uint16(d[i:])
					if isGrease(v) {
						ch.Grease = true
						continue
					}
					ch.SupportedVersions = append(ch.SupportedVersions, v)
				}
			}
		case 0x0033: // key_share
			if len(data) >= 2 {
				n := int(binary.BigEndian.Uint16(data))
				d := data[2:]
				for len(d) >= 4 && n >= 4 {
					group := binary.BigEndian.Uint16(d)
					kl := int(binary.BigEndian.Uint16(d[2:]))
					if len(d) < 4+kl {
						break
					}
					if group == 0x11ec || group == 0x6399 || group == 0x639a { // X25519MLKEM768, Kyber768
						ch.PQ = true
					}
					d = d[4+kl:]
					n -= 4 + kl
				}
			}
		case 0xfe0d: // encrypted_client_hello (GREASE-ECH included)
			ch.ECH = true
		}
	}
	ch.Ciphers, ch.Extensions = len(ciphers), len(exts)
	sort.Strings(ciphers)
	sort.Strings(exts)
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
	ch.JA4 = fmt.Sprintf("%s13%s%02d%02d%s_%s_%s", proto, sni, min(ch.Ciphers, 99), min(ch.Extensions, 99), alpn,
		hashList(ciphers, 6), hashList(sigList(ch), 6))
	ch.Print = ch.JA4
	ch.JA3 = ja3(ch)
	return ch, true
}

func sigList(ch clientHello) []string {
	out := make([]string, 0, len(ch.SigAlgs))
	for _, v := range ch.SigAlgs {
		out = append(out, fmt.Sprintf("%04x", v))
	}
	sort.Strings(out)
	return out
}

func hashList(list []string, bytesN int) string {
	sum := sha256.Sum256([]byte(strings.Join(list, ",")))
	return hex.EncodeToString(sum[:bytesN])
}

func joinDec[T ~uint16 | ~byte](list []T) string {
	if len(list) == 0 {
		return ""
	}
	parts := make([]string, len(list))
	for i, v := range list {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, "-")
}

// ja3 is the classic JA3 label: version,ciphers,extensions,curves,point formats (decimal),
// hashed with MD5. GREASE is left out by the parser.
func ja3(ch clientHello) string {
	raw := fmt.Sprintf("%d,%s,%s,%s,%s", ch.Version,
		joinDec(ch.CipherList), joinDec(ch.ExtList), joinDec(ch.Groups), joinDec(ch.PointFormats))
	sum := md5.Sum([]byte(raw)) // #nosec G401 -- a fingerprint label, not a security primitive
	return hex.EncodeToString(sum[:])
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
// bits are not near half ones per byte, when it starts with or mostly is printable text, or when it
// starts as TLS or HTTP. What is left looks like nothing but randomness, which is what GFW
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

// shannon is the per-byte Shannon entropy of a UDP payload, in bits (0..8).
func shannon(p []byte) float64 {
	if len(p) == 0 {
		return 0
	}
	var counts [256]int
	for _, c := range p {
		counts[c]++
	}
	var e float64
	n := float64(len(p))
	for _, c := range counts {
		if c == 0 {
			continue
		}
		pr := float64(c) / n
		e -= pr * math.Log2(pr)
	}
	return e
}

func popAvg(p []byte) float64 {
	if len(p) == 0 {
		return 0
	}
	ones := 0
	for _, c := range p {
		ones += bits.OnesCount8(c)
	}
	return float64(ones) / float64(len(p))
}
