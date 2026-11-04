//go:build linux || darwin || freebsd

package udpsock

import "testing"

// FuzzControlMessages feeds arbitrary control-message bytes to the parsers that run on every
// received and sent datagram. A malformed message from the kernel or a caller must be ignored,
// never panic.
func FuzzControlMessages(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 32))
	f.Add(marshalTOS(2))
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = stripECN(nil, b)
		_ = hasECN(b)
		_ = gsoSegment(b)
		_ = dropGSO(nil, b)
	})
}
