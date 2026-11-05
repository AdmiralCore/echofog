package selfcover

import (
	"net"
	"testing"
	"time"
)

// FuzzFrontReadFirst feeds arbitrary bytes as the first flight of a connection: readFirst must
// return, never panic, and return every byte it consumed so that the website sees the whole
// stream.
func FuzzFrontReadFirst(f *testing.F) {
	f.Add([]byte{0x16, 3, 1, 0, 4, 1, 0, 0, 0})
	f.Add([]byte{0x16, 3, 1, 0x40, 0x01})
	f.Add([]byte{0x17, 3, 3, 0, 1, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		fr := &front{timeout: 50 * time.Millisecond}
		go func() {
			_, _ = b.Write(data)
			_ = b.Close()
		}()
		buf, hello := fr.readFirst(a)
		if len(buf) > len(data) {
			t.Fatalf("returned %d bytes of %d", len(buf), len(data))
		}
		if string(buf) != string(data[:len(buf)]) {
			t.Fatal("returned bytes differ from the stream")
		}
		_ = hello
	})
}
