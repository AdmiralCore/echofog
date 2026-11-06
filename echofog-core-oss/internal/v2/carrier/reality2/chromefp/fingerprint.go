// Package chromefp compares the ClientHello our uTLS profile sends with the one a live stable
// Chrome sends (T2.6d, H6). A censor that knows Chrome's current hello treats a client whose hello
// lags a version as a mismatch (the V2Ray/JA3 history), so the profile has to follow Chrome.
//
// A Sample is the parsed, secret-free shape of one ClientHello — TCP or QUIC Initial: JA4, cipher
// suites, the extension set, supported groups, signature algorithms, the key shares with their
// sizes (the post-quantum share included), ALPN, QUIC transport parameter ids and the places GREASE
// occupies. Random values (client random, session id, key share bytes, ECH payload) are never
// stored. Chrome permutes its extension order per connection, so the order is compared as a set
// and, separately, the first and last extension positions that GREASE holds.
//
// Compare lists the differences between two samples; an empty list means the profile matches.
// The live capture (headless Chrome against a loopback server) lives in the tests; CI compares
// our profile with the stored sample, so it needs no browser.
package chromefp

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// GREASE stands for every GREASE value (RFC 8701) in a sample.
const GREASE = 0x0a0a

// Sample is the stored shape of one ClientHello.
type Sample struct {
	// Source tells where it came from: "chrome" (live capture) or "ours" (our profile).
	Source string `json:"source"`
	// Chrome is the browser version a live capture was taken from.
	Chrome string `json:"chrome,omitempty"`
	// Transport is "tcp" or "quic".
	Transport string `json:"transport"`

	JA4          string   `json:"ja4"`
	JA4Raw       string   `json:"ja4_raw"`
	SessionIDLen int      `json:"session_id_len"`
	Ciphers      []uint16 `json:"ciphers"`
	// Extensions are the extension types sorted, GREASE as 0x0a0a. The order Chrome sends them in
	// differs per connection.
	Extensions []uint16 `json:"extensions"`
	// GreaseFirst and GreaseLast report GREASE as the first and the last extension.
	GreaseFirst bool     `json:"grease_first"`
	GreaseLast  bool     `json:"grease_last"`
	Groups      []uint16 `json:"groups"`
	SigAlgs     []uint16 `json:"sig_algs"`
	Versions    []uint16 `json:"versions"`
	ALPN        []string `json:"alpn"`
	PSKModes    []uint8  `json:"psk_modes,omitempty"`
	ECPoints    []uint8  `json:"ec_point_formats,omitempty"`
	CertCompr   []uint16 `json:"cert_compression,omitempty"`
	// ALPS are the application protocols of the ALPS extension (17613 or 17513).
	ALPS []string `json:"alps,omitempty"`
	// KeyShares are the offered shares in order: group and length of the public value.
	KeyShares []KeyShare `json:"key_shares"`
	// ECH describes the encrypted_client_hello extension: its outer type and payload length.
	ECH *ECH `json:"ech,omitempty"`
	// PaddingPresent reports the padding extension.
	PaddingPresent bool `json:"padding_present"`
	// TransportParams are the QUIC transport parameters (QUIC only), GREASE ids as 0x0a0a.
	TransportParams []TP `json:"transport_params,omitempty"`
	// ExtraExtensionBodies keep the body length of extensions whose length is stable in Chrome.
	BodyLens map[uint16]int `json:"body_lens,omitempty"`
}

// KeyShare is one offered key share.
type KeyShare struct {
	Group uint16 `json:"group"`
	Len   int    `json:"len"`
}

// ECH is the shape of the encrypted_client_hello extension.
type ECH struct {
	OuterType  uint8  `json:"outer_type"`
	KDF        uint16 `json:"kdf"`
	AEAD       uint16 `json:"aead"`
	PayloadLen int    `json:"payload_len"`
}

// TP is a QUIC transport parameter: id, value length and, for small integers, the value.
type TP struct {
	ID    uint64 `json:"id"`
	Len   int    `json:"len"`
	Value uint64 `json:"value,omitempty"`
}

// ErrMalformed reports bytes that are not a ClientHello.
var ErrMalformed = errors.New("chromefp: malformed ClientHello")

func isGrease(v uint16) bool { return v&0x0f0f == 0x0a0a && v>>8 == v&0xff }

func norm(v uint16) uint16 {
	if isGrease(v) {
		return GREASE
	}
	return v
}

type reader struct {
	b   []byte
	err bool
}

func (r *reader) take(n int) []byte {
	if n < 0 || len(r.b) < n {
		r.err = true
		r.b = nil
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) u8() uint8 {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *reader) u16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}

func (r *reader) vec16() []byte { return r.take(int(r.u16())) }
func (r *reader) vec8() []byte  { return r.take(int(r.u8())) }

func u16s(b []byte) []uint16 {
	var out []uint16
	for i := 0; i+1 < len(b); i += 2 {
		out = append(out, binary.BigEndian.Uint16(b[i:]))
	}
	return out
}

func varint(b []byte) (uint64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	n := 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, 0, false
	}
	v := uint64(b[0] & 0x3f)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n, true
}

// Parse reads a ClientHello handshake message (type, 3-byte length, body) into a Sample. The
// transport is "quic" when the hello carries the quic_transport_parameters extension.
func Parse(msg []byte) (*Sample, error) {
	r := &reader{b: msg}
	if r.u8() != 1 {
		return nil, ErrMalformed
	}
	n := int(r.u8())<<16 | int(r.u8())<<8 | int(r.u8())
	body := r.take(n)
	if r.err {
		return nil, ErrMalformed
	}
	r = &reader{b: body}
	r.u16() // legacy_version
	r.take(32)
	s := &Sample{Transport: "tcp", BodyLens: map[uint16]int{}}
	s.SessionIDLen = len(r.vec8())
	for _, c := range u16s(r.vec16()) {
		s.Ciphers = append(s.Ciphers, norm(c))
	}
	r.vec8() // compression methods
	exts := &reader{b: r.vec16()}
	if r.err {
		return nil, ErrMalformed
	}
	var order []uint16
	for len(exts.b) > 0 && !exts.err {
		t := exts.u16()
		data := exts.vec16()
		if exts.err {
			return nil, ErrMalformed
		}
		order = append(order, norm(t))
		if err := s.extension(t, data); err != nil {
			return nil, err
		}
	}
	if len(order) == 0 {
		return nil, ErrMalformed
	}
	s.GreaseFirst, s.GreaseLast = order[0] == GREASE, order[len(order)-1] == GREASE
	s.Extensions = slices.Clone(order)
	slices.Sort(s.Extensions)
	s.ja4()
	return s, nil
}

func (s *Sample) extension(t uint16, d []byte) error {
	r := &reader{b: d}
	switch t {
	case 0x000a: // supported_groups
		for _, g := range u16s(r.vec16()) {
			s.Groups = append(s.Groups, norm(g))
		}
	case 0x000d: // signature_algorithms
		for _, a := range u16s(r.vec16()) {
			s.SigAlgs = append(s.SigAlgs, norm(a))
		}
	case 0x002b: // supported_versions
		for _, v := range u16s(r.vec8()) {
			s.Versions = append(s.Versions, norm(v))
		}
	case 0x0010: // ALPN
		l := &reader{b: r.vec16()}
		for len(l.b) > 0 && !l.err {
			s.ALPN = append(s.ALPN, string(l.vec8()))
		}
	case 0x002d:
		s.PSKModes = slices.Clone(r.vec8())
	case 0x000b:
		s.ECPoints = slices.Clone(r.vec8())
	case 0x001b: // compress_certificate
		for _, a := range u16s(r.vec8()) {
			s.CertCompr = append(s.CertCompr, a)
		}
	case 0x4469, 0x44cd: // ALPS (new and old codepoint)
		l := &reader{b: r.vec16()}
		for len(l.b) > 0 && !l.err {
			s.ALPS = append(s.ALPS, string(l.vec8()))
		}
	case 0x0033: // key_share
		l := &reader{b: r.vec16()}
		for len(l.b) > 0 && !l.err {
			g := norm(l.u16())
			s.KeyShares = append(s.KeyShares, KeyShare{Group: g, Len: len(l.vec16())})
		}
	case 0xfe0d: // encrypted_client_hello
		e := &ECH{OuterType: r.u8()}
		if e.OuterType == 0 {
			e.KDF, e.AEAD = r.u16(), r.u16()
			r.u8()
			r.vec16()
			e.PayloadLen = len(r.vec16())
		}
		if r.err {
			return ErrMalformed
		}
		s.ECH = e
	case 0x0015:
		s.PaddingPresent = true
	case 0x0039, 0xffa5: // quic_transport_parameters
		s.Transport = "quic"
		return s.quicParams(d)
	}
	if r.err {
		return ErrMalformed
	}
	switch t {
	case 0x0000, 0x0010, 0x0033, 0x0015, 0xfe0d, 0x0039, 0x0029, 0x4469, 0x44cd:
	default:
		if !isGrease(t) {
			s.BodyLens[t] = len(d)
		}
	}
	return nil
}

func (s *Sample) quicParams(d []byte) error {
	for len(d) > 0 {
		id, n, ok := varint(d)
		if !ok {
			return ErrMalformed
		}
		d = d[n:]
		l, n, ok := varint(d)
		if !ok || uint64(len(d)-n) < l {
			return ErrMalformed
		}
		val := d[n : n+int(l)]
		d = d[n+int(l):]
		tp := TP{ID: id, Len: int(l)}
		if id%31 == 27 { // GREASE id (RFC 9000 §18.1)
			tp.ID = GREASE
			tp.Len = 0
		} else if v, m, ok := varint(val); ok && m == len(val) && id < 0x100 {
			tp.Value = v
		}
		s.TransportParams = append(s.TransportParams, tp)
	}
	sort.SliceStable(s.TransportParams, func(i, j int) bool { return s.TransportParams[i].ID < s.TransportParams[j].ID })
	return nil
}

// ja4 fills JA4 and JA4Raw (FoxIO JA4: version, SNI, counts, ALPN, then hashes of the sorted
// ciphers and of the sorted extensions plus the signature algorithms in order).
func (s *Sample) ja4() {
	proto := "t"
	if s.Transport == "quic" {
		proto = "q"
	}
	ver := "13"
	if !slices.Contains(s.Versions, 0x0304) {
		ver = "12"
	}
	var ciphers []string
	for _, c := range s.Ciphers {
		if c != GREASE {
			ciphers = append(ciphers, fmt.Sprintf("%04x", c))
		}
	}
	var exts []string
	count, sni := 0, "i"
	for _, e := range s.Extensions {
		if e == GREASE {
			continue
		}
		count++
		if e == 0x0000 {
			sni = "d"
			continue
		}
		if e == 0x0010 {
			continue
		}
		exts = append(exts, fmt.Sprintf("%04x", e))
	}
	alpn := "00"
	if len(s.ALPN) > 0 && s.ALPN[0] != "" {
		a := s.ALPN[0]
		alpn = string(a[0]) + string(a[len(a)-1])
	}
	var sigs []string
	for _, a := range s.SigAlgs {
		sigs = append(sigs, fmt.Sprintf("%04x", a))
	}
	sortedCiphers := slices.Clone(ciphers)
	sort.Strings(sortedCiphers)
	c := strings.Join(sortedCiphers, ",")
	e := strings.Join(exts, ",") + "_" + strings.Join(sigs, ",")
	a := fmt.Sprintf("%s%s%s%02d%02d%s", proto, ver, sni, len(ciphers), count, alpn)
	s.JA4Raw = a + "_" + c + "_" + e
	s.JA4 = a + "_" + h12(c) + "_" + h12(e)
}

func h12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// Compare lists how got differs from want, one line per difference; empty means equal. want is
// normally the live Chrome sample, got our profile.
func Compare(want, got *Sample) []string {
	var d []string
	add := func(name string, w, g any) {
		if fmt.Sprint(w) != fmt.Sprint(g) {
			d = append(d, fmt.Sprintf("%s: chrome %v, ours %v", name, w, g))
		}
	}
	add("transport", want.Transport, got.Transport)
	add("ja4", want.JA4, got.JA4)
	add("session_id_len", want.SessionIDLen, got.SessionIDLen)
	add("ciphers", hexs(want.Ciphers), hexs(got.Ciphers))
	add("extensions", hexs(want.Extensions), hexs(got.Extensions))
	add("grease_first", want.GreaseFirst, got.GreaseFirst)
	add("grease_last", want.GreaseLast, got.GreaseLast)
	add("groups", hexs(want.Groups), hexs(got.Groups))
	add("sig_algs", hexs(want.SigAlgs), hexs(got.SigAlgs))
	add("versions", hexs(want.Versions), hexs(got.Versions))
	add("alpn", want.ALPN, got.ALPN)
	add("psk_modes", want.PSKModes, got.PSKModes)
	add("ec_point_formats", want.ECPoints, got.ECPoints)
	add("cert_compression", hexs(want.CertCompr), hexs(got.CertCompr))
	add("alps", want.ALPS, got.ALPS)
	add("key_shares", want.KeyShares, got.KeyShares)
	// the ECH payload length is drawn at random per connection (144..240 in Chrome): not compared
	add("ech", echShape(want.ECH), echShape(got.ECH))
	add("padding_present", want.PaddingPresent, got.PaddingPresent)
	add("transport_params", want.TransportParams, got.TransportParams)
	add("body_lens", fmt.Sprint(want.BodyLens), fmt.Sprint(got.BodyLens))
	return d
}

func hexs(v []uint16) string {
	var b []string
	for _, x := range v {
		b = append(b, fmt.Sprintf("%04x", x))
	}
	return strings.Join(b, ",")
}

func echShape(e *ECH) string {
	if e == nil {
		return "none"
	}
	return fmt.Sprintf("outer=%d kdf=%d aead=%d", e.OuterType, e.KDF, e.AEAD)
}
