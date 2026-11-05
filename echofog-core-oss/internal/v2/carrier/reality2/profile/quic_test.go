package profile

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"testing"

	"golang.org/x/crypto/hkdf"
)

// sealServerInitial builds a protected QUIC v1 server Initial packet (RFC 9001 §5) whose
// payload is an ACK-free CRYPTO frame at offset off carrying data, padded to 1200 bytes.
func sealServerInitial(t *testing.T, clientDCID, scid []byte, off int, data []byte) []byte {
	t.Helper()
	secret := hkdf.Extract(sha256.New, clientDCID, quicV1Salt)
	s := expandLabel(secret, "server in", 32)
	key, iv, hp := expandLabel(s, "quic key", 16), expandLabel(s, "quic iv", 12), expandLabel(s, "quic hp", 16)

	frame := []byte{0x06, byte(off)} // CRYPTO, offset < 64
	frame = append(frame, 0x40|byte(len(data)>>8), byte(len(data)))
	frame = append(frame, data...)
	for len(frame) < 1100 {
		frame = append(frame, 0) // PADDING
	}
	hdr := []byte{0xc0, 0, 0, 0, 1, 0, byte(len(scid))}
	hdr = append(hdr, scid...)
	hdr = append(hdr, 0) // token length
	length := 1 + len(frame) + 16
	hdr = append(hdr, 0x40|byte(length>>8), byte(length))
	pnOff := len(hdr)
	hdr = append(hdr, 0) // packet number 0, one byte

	blk, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(blk)
	sealed := gcm.Seal(nil, iv, frame, hdr) // nonce = iv xor 0
	pkt := append(append([]byte(nil), hdr...), sealed...)

	hb, _ := aes.NewCipher(hp)
	var mask [16]byte
	hb.Encrypt(mask[:], pkt[pnOff+4:pnOff+20])
	pkt[0] ^= mask[0] & 0x0f
	pkt[pnOff] ^= mask[1]
	return pkt
}

func clientInitial(dcid []byte) []byte {
	b := []byte{0xc0, 0, 0, 0, 1, byte(len(dcid))}
	b = append(b, dcid...)
	b = append(b, 0, 0) // SCID length, token length
	payload := make([]byte, 1100)
	b = append(b, 0x44, 0x4c) // Length 1100
	return append(b, payload...)
}

// The server's Initial packets are keyed by the client's first DCID. A client Initial that
// follows the server's first packet carries the server's SCID as DCID and must not change the
// key (seen against Cloudflare: an ACK-only Initial arrives before the ServerHello).
func TestObserveQUICKeepsFirstDCID(t *testing.T) {
	dcidA := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	scid := []byte{9, 9, 9, 9}
	sh := buildServerHello(false, 0x1301, 0x001d, []uint16{51, 43}, 0)
	// the ServerHello split over two packets, delivered in two datagrams
	half := len(sh) / 2
	events := []Event{
		{Data: clientInitial(dcidA)},
		{FromServer: true, Data: sealServerInitial(t, dcidA, scid, 0, nil)},
		{Data: clientInitial(scid)},
		{FromServer: true, Data: sealServerInitial(t, dcidA, scid, 0, sh[:half])},
		{FromServer: true, Data: sealServerInitial(t, dcidA, scid, half, sh[half:])},
	}
	v, err := ObserveQUIC(events)
	if err != nil {
		t.Fatal(err)
	}
	if v.Group != 0x1d || v.Cipher != 0x1301 || v.SCIDLen != 4 || len(v.Flight) != 3 || v.Flight[1].Unit != 1 {
		t.Fatalf("%+v", v)
	}
}
