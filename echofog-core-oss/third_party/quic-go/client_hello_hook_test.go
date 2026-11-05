package quic

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"
)

// TestClientHelloHook covers fork patch 4 (ECHOFOG-PATCH.md): the hook receives CH0 (the exact
// ClientHello with a zeroed random) and the private X25519 key of the hello's X25519 share, and
// its 32 bytes become the random of the hello that is actually hashed and sent.
func TestClientHelloHook(t *testing.T) {
	cert := testServerCertificate(t)
	ln, err := ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h3"},
	}, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	accepted := make(chan error, 1)
	go func() {
		conn, err := ln.Accept(ctx)
		if err == nil {
			_ = conn.CloseWithError(0, "")
		}
		accepted <- err
	}()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingPacketConn{PacketConn: udp}
	defer udp.Close()

	want := [32]byte{}
	for i := range want {
		want[i] = byte(0xa0 + i)
	}
	var gotCH0, gotC []byte
	calls := 0
	conf := &Config{
		ChromeParrot:    true,
		EnableDatagrams: true,
		ClientHelloHook: func(ch0, c []byte) ([32]byte, error) {
			calls++
			gotCH0 = append([]byte(nil), ch0...)
			gotC = append([]byte(nil), c...)
			return want, nil
		},
	}
	clientTLS := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}}
	conn, err := Dial(ctx, rec, ln.Addr(), clientTLS, conf)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.CloseWithError(0, "")
	if err := <-accepted; err != nil {
		t.Fatalf("accept: %v", err)
	}

	if calls != 1 {
		t.Fatalf("hook called %d times, want once", calls)
	}
	if len(gotCH0) < 38 || gotCH0[0] != 1 {
		t.Fatalf("hook got %d bytes that are not a ClientHello", len(gotCH0))
	}
	bodyLen := int(gotCH0[1])<<16 | int(gotCH0[2])<<8 | int(gotCH0[3])
	if bodyLen+4 != len(gotCH0) {
		t.Fatalf("CH0 length field %d, message %d bytes", bodyLen, len(gotCH0))
	}
	for _, b := range gotCH0[6:38] {
		if b != 0 {
			t.Fatal("CH0 random is not zeroed")
		}
	}

	// The hello that went on the wire is CH0 with the hook's bytes in the random, so the server
	// and the client hashed the same message.
	wireHello := clientHelloFromInitials(t, rec.snapshot())
	if !bytes.Equal(wireHello[6:38], want[:]) {
		t.Fatalf("wire random = %x, want the hook's %x", wireHello[6:38], want)
	}
	zeroed := append([]byte(nil), wireHello...)
	clear(zeroed[6:38])
	if !bytes.Equal(zeroed, gotCH0) {
		t.Fatal("wire hello with a zeroed random differs from the hook's CH0")
	}

	// The hook's private key is the one behind the hello's key share.
	if len(gotC) != 32 {
		t.Fatalf("hook got a %d-byte private key", len(gotC))
	}
	priv, err := ecdh.X25519().NewPrivateKey(gotC)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyShareC(t, gotCH0), priv.PublicKey().Bytes()) {
		t.Fatal("the hook's private key does not match the hello's X25519 key share")
	}
}

// TestClientHelloHookError checks that a failing hook fails the dial instead of sending a hello
// without a token.
func TestClientHelloHookError(t *testing.T) {
	cert := testServerCertificate(t)
	ln, err := ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h3"},
	}, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conf := &Config{
		ChromeParrot:    true,
		EnableDatagrams: true,
		ClientHelloHook: func([]byte, []byte) ([32]byte, error) {
			return [32]byte{}, errTestHook
		},
	}
	if _, err := DialAddr(ctx, ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}}, conf); err == nil {
		t.Fatal("dial succeeded with a failing ClientHello hook")
	}
}

type testHookError struct{}

func (testHookError) Error() string { return "test: hook failed" }

var errTestHook = testHookError{}

// recordingPacketConn keeps every datagram the client sends, so the test can read the ClientHello
// off the wire.
type recordingPacketConn struct {
	net.PacketConn
	mu   sync.Mutex
	sent [][]byte
}

func (c *recordingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	c.sent = append(c.sent, append([]byte(nil), p...))
	c.mu.Unlock()
	return c.PacketConn.WriteTo(p, addr)
}

func (c *recordingPacketConn) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.sent...)
}

// clientHelloFromInitials decrypts the client's QUIC v1 Initial packets and reassembles the
// ClientHello from their CRYPTO frames (RFC 9001 §5.2, RFC 9000 §19).
func clientHelloFromInitials(t *testing.T, packets [][]byte) []byte {
	t.Helper()
	frags := map[int][]byte{}
	var dcid []byte
	for _, p := range packets {
		var ok bool
		dcid, frags, ok = initialCrypto(p, dcid, frags)
		if !ok {
			continue
		}
	}
	var out []byte
	for off := 0; ; {
		b, ok := frags[off]
		if !ok || len(b) == 0 {
			break
		}
		out = append(out, b...)
		off += len(b)
	}
	if len(out) < 4 || out[0] != 1 {
		t.Fatalf("reassembled %d bytes that are not a ClientHello", len(out))
	}
	return out
}

var quicV1Salt = []byte{
	0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3,
	0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad,
	0xcc, 0xbb, 0x7f, 0x0a,
}

func initialCrypto(pkt, dcid []byte, frags map[int][]byte) ([]byte, map[int][]byte, bool) {
	if len(pkt) < 7 || pkt[0]&0x80 == 0 || binary.BigEndian.Uint32(pkt[1:5]) != 1 || (pkt[0]>>4)&0x03 != 0 {
		return dcid, frags, false
	}
	off := 5
	n := int(pkt[off])
	off++
	if n > 20 || len(pkt) < off+n+1 {
		return dcid, frags, false
	}
	if dcid == nil {
		dcid = append([]byte(nil), pkt[off:off+n]...)
	}
	off += n
	n = int(pkt[off])
	off++
	if n > 20 || len(pkt) < off+n {
		return dcid, frags, false
	}
	off += n // SCID
	tokLen, k, ok := readVarint(pkt[off:])
	if !ok || uint64(len(pkt)-off-k) < tokLen {
		return dcid, frags, false
	}
	off += k + int(tokLen)
	length, k, ok := readVarint(pkt[off:])
	if !ok || length < 20 {
		return dcid, frags, false
	}
	off += k
	pnOff := off
	end := pnOff + int(length)
	if end > len(pkt) || pnOff+20 > len(pkt) {
		return dcid, frags, false
	}
	secret := hkdfExpandLabel(hkdf.Extract(sha256.New, dcid, quicV1Salt), "client in", 32)
	key := hkdfExpandLabel(secret, "quic key", 16)
	iv := hkdfExpandLabel(secret, "quic iv", 12)
	hp := hkdfExpandLabel(secret, "quic hp", 16)
	hb, _ := aes.NewCipher(hp)
	mask := make([]byte, 16)
	hb.Encrypt(mask, pkt[pnOff+4:pnOff+20])
	hdr0 := pkt[0] ^ (mask[0] & 0x0f)
	pnLen := int(hdr0&0x03) + 1
	if pnOff+pnLen > end {
		return dcid, frags, false
	}
	hdr := append([]byte(nil), pkt[:pnOff+pnLen]...)
	hdr[0] = hdr0
	var pn uint64
	for i := 0; i < pnLen; i++ {
		hdr[pnOff+i] ^= mask[1+i]
		pn = pn<<8 | uint64(hdr[pnOff+i])
	}
	var nonce [12]byte
	copy(nonce[:], iv)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}
	kb, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(kb)
	plain, err := aead.Open(nil, nonce[:], pkt[pnOff+pnLen:end], hdr)
	if err != nil {
		return dcid, frags, false
	}
	if frags == nil {
		frags = map[int][]byte{}
	}
	for p := 0; p < len(plain); {
		ft := plain[p]
		if ft == 0x00 || ft == 0x01 {
			p++
			continue
		}
		if ft != 0x06 {
			break
		}
		p++
		fo, n1, ok := readVarint(plain[p:])
		if !ok {
			break
		}
		p += n1
		fl, n2, ok := readVarint(plain[p:])
		if !ok || uint64(len(plain)-p-n2) < fl {
			break
		}
		p += n2
		if _, seen := frags[int(fo)]; !seen {
			frags[int(fo)] = append([]byte(nil), plain[p:p+int(fl)]...)
		}
		p += int(fl)
	}
	return dcid, frags, true
}

func readVarint(b []byte) (uint64, int, bool) {
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

func hkdfExpandLabel(secret []byte, label string, length int) []byte {
	full := "tls13 " + label
	info := binary.BigEndian.AppendUint16(nil, uint16(length))
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, 0)
	out := make([]byte, length)
	_, _ = io.ReadFull(hkdf.Expand(sha256.New, secret, info), out)
	return out
}

// keyShareC returns the X25519 public key of the hello's X25519 key share.
func keyShareC(t *testing.T, msg []byte) []byte {
	t.Helper()
	p := 4 + 2 + 32
	p += 1 + int(msg[p])
	p += 2 + (int(msg[p])<<8 | int(msg[p+1]))
	p += 1 + int(msg[p])
	end := p + 2 + (int(msg[p])<<8 | int(msg[p+1]))
	for p += 2; p+4 <= end; {
		typ := int(msg[p])<<8 | int(msg[p+1])
		l := int(msg[p+2])<<8 | int(msg[p+3])
		p += 4
		if typ == 51 {
			body := msg[p : p+l]
			for q := 2; q+4 <= len(body); {
				group := int(body[q])<<8 | int(body[q+1])
				kl := int(body[q+2])<<8 | int(body[q+3])
				q += 4
				if group == 0x001d && kl == 32 {
					return append([]byte(nil), body[q:q+kl]...)
				}
				q += kl
			}
		}
		p += l
	}
	t.Fatal("hello has no X25519 key share")
	return nil
}

func testServerCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}
