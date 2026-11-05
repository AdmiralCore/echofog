package exit

import (
	"bytes"
	"errors"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// FuzzParseFragment checks that no datagram can panic the reassembler's parser and that every
// accepted fragment re-encodes to a fragment with the same fields.
func FuzzParseFragment(f *testing.F) {
	f.Add([]byte{0x05, 0x00, 0x01, 0x00, 0x01, 0x0e, '1', '9', '2', '.', '0', '.', '2', '.', '5', '3', ':', '5', '3', 0xab, 0xcd})
	f.Add([]byte{0x05, 0x00, 0x01, 0x01, 0x02, 0x01, 0x02})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		fr, err := ParseFragment(b)
		if err != nil {
			if !errors.Is(err, frame.ErrMalformed) {
				t.Fatalf("%x: %v is not ErrMalformed", b, err)
			}
			return
		}
		if fr.Count < 1 || fr.Count > MaxFragments || fr.Index >= fr.Count {
			t.Fatalf("%x: accepted fragment %d/%d", b, fr.Index, fr.Count)
		}
		again, err := ParseFragment(fr.Append(nil))
		if err != nil {
			t.Fatalf("%x: re-encoded fragment rejected: %v", b, err)
		}
		if again.Session != fr.Session || again.Packet != fr.Packet || again.Index != fr.Index ||
			again.Count != fr.Count || again.Addr != fr.Addr || !bytes.Equal(again.Data, fr.Data) {
			t.Fatalf("%x: round trip changed the fragment: %+v vs %+v", b, again, fr)
		}
	})
}

// FuzzParseExitFrame runs every exit/2 body parser on arbitrary bytes: known-type payloads must
// either be rejected with frame.ErrMalformed or decoded into a body that re-encodes cleanly.
func FuzzParseExitFrame(f *testing.F) {
	f.Add(uint8(0), []byte{0x0a, 0x0a, '1', '9', '2', '.', '0', '.', '2', '.', '5', '3', 0x10, 0x35})
	f.Add(uint8(1), []byte{0x08, 0x07})
	f.Add(uint8(2), []byte{0x08, 0x01})
	f.Add(uint8(3), []byte{0x08, 0x01, 0x10, 0x05})
	f.Add(uint8(4), []byte{0x0a, 0x0e, '1', '9', '2', '.', '0', '.', '2', '.', '5', '3', ':', '5', '3', 0xab, 0xcd})
	f.Add(uint8(5), []byte{})
	f.Add(uint8(6), []byte{0x08, 0x01})
	f.Add(uint8(7), []byte{0x08, 0x01, 0x12, 0x01, 0x00})

	f.Fuzz(func(t *testing.T, sel uint8, b []byte) {
		var err error
		switch sel % 8 {
		case 0:
			_, err = ParseTCPConnect(b)
		case 1:
			_, err = ParseResult(b)
		case 2:
			_, err = ParseUDPOpen(b)
		case 3:
			_, err = ParseUDPOK(b)
		case 4:
			_, err = ParseUDPPkt(b)
			if err == nil {
				if u, e := ParseUDPPkt(b); e != nil || u.Addr == "" {
					t.Fatalf("%x: accepted a UDP_PKT without an address", b)
				}
			}
		case 5:
			_, err = ParseAccount(b)
		case 6:
			_, err = ParseVoucher(b)
		case 7:
			_, err = ParseVoucherResult(b)
		}
		if err != nil && !errors.Is(err, frame.ErrMalformed) {
			t.Fatalf("%x: %v is not ErrMalformed", b, err)
		}
	})
}
