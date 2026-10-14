package exit_gateway

import (
	"bytes"
	"testing"
)

// FuzzReadSocksAddr: the SOCKS5 address of a request comes from whatever program is on localhost
// (and, through the UDP relay, from anywhere that can reach the socket).
func FuzzReadSocksAddr(f *testing.F) {
	f.Add([]byte{0x01, 127, 0, 0, 1, 0x1f, 0x90})
	f.Add(append([]byte{0x03, 11}, append([]byte("example.com"), 0, 80)...))
	f.Add(append([]byte{0x04}, append(make([]byte, 16), 0, 80)...))
	f.Add([]byte{0x05})
	f.Add([]byte{})
	f.Add([]byte{0x03, 255})
	f.Fuzz(func(t *testing.T, data []byte) {
		addr, code, err := readSocksAddr(bytes.NewReader(data))
		if err == nil && (addr == "" || code != 0) {
			t.Fatalf("success with address %q and code %d", addr, code)
		}
	})
}

func FuzzParseUDPRequest(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0x01, 8, 8, 8, 8, 0, 53, 'h', 'i'})
	f.Add([]byte{0, 0, 1, 0x01, 8, 8, 8, 8, 0, 53}) // fragmentation: refused
	f.Add([]byte{0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		addr, raw, payload, ok := parseUDPRequest(b)
		if !ok {
			return
		}
		if addr == "" || len(raw) == 0 || len(raw)+len(payload)+3 != len(b) {
			t.Fatalf("pieces do not add up: addr=%q raw=%d payload=%d total=%d", addr, len(raw), len(payload), len(b))
		}
	})
}

// FuzzDatagram: the exit's UDP relay frames. What decodes must encode back to the same bytes.
func FuzzDatagram(f *testing.F) {
	seed, _ := encodeDatagram("example.com:53", []byte("query"))
	f.Add(seed)
	f.Add([]byte{0, 0})
	f.Add([]byte{0xff, 0xff, 1})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, frame []byte) {
		addr, payload, err := decodeDatagram(frame)
		if err != nil {
			return
		}
		again, err := encodeDatagram(addr, payload)
		if err != nil {
			return // a payload over the limit may decode but is not re-encoded
		}
		if !bytes.Equal(again, frame) {
			t.Fatalf("round trip changed the datagram")
		}
	})
}
