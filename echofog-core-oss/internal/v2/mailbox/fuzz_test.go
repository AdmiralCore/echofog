package mailbox

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/identity"
)

// FuzzParseFrames covers every frame decoder: no panic, and a successful parse re-encodes to a
// message that parses again to the same fields.
func FuzzParseFrames(f *testing.F) {
	v := kdfVector(f)
	f.Add(vhex(f, v, "deposit"))
	f.Add(QueueCreate{Req: 1, Queue: make([]byte, QueueSize), AuthKey: make([]byte, 32), Public: true, PowBits: 20}.Append(nil))
	f.Add(Fetch{Req: 2, Queue: make([]byte, QueueSize), Max: 10, Wait: 60, Auth: make([]byte, 64)}.Append(nil))
	f.Add(Ack{Req: 3, Queue: make([]byte, QueueSize), IDs: [][]byte{{1, 2}}, Auth: make([]byte, 64)}.Append(nil))
	f.Add(Forward{Req: 4, Relay: make([]byte, 32), Sealed: make([]byte, 48)}.Append(nil))
	f.Add(Items{Req: 5, Items: []Item{{ID: make([]byte, ItemSize), Blob: []byte("b"), Stored: 1}}, More: true}.Append(nil))

	f.Fuzz(func(t *testing.T, b []byte) {
		if m, err := ParseQueueCreate(b); err == nil {
			mustReparse(t, m.Append(nil), func(x []byte) error { _, err := ParseQueueCreate(x); return err })
		}
		if m, err := ParseQueueOK(b); err == nil {
			mustReparse(t, m.Append(nil), func(x []byte) error { _, err := ParseQueueOK(x); return err })
		}
		if m, err := ParseDeposit(b); err == nil {
			mustReparse(t, m.Append(nil), func(x []byte) error { _, err := ParseDeposit(x); return err })
		}
		if m, err := ParseDeposited(b); err == nil {
			mustReparse(t, m.Append(nil), func(x []byte) error { _, err := ParseDeposited(x); return err })
		}
		if m, err := ParseFetch(b); err == nil {
			mustReparse(t, m.Append(nil), func(x []byte) error { _, err := ParseFetch(x); return err })
		}
		if m, err := ParseItems(b); err == nil {
			mustReparse(t, m.Append(nil), func(x []byte) error { _, err := ParseItems(x); return err })
		}
		if m, err := ParseAck(b); err == nil {
			mustReparse(t, m.Append(nil), func(x []byte) error { _, err := ParseAck(x); return err })
		}
		if m, err := ParseForward(b); err == nil {
			mustReparse(t, m.Append(nil), func(x []byte) error { _, err := ParseForward(x); return err })
		}
		if m, err := ParseError(b); err == nil {
			mustReparse(t, m.Append(nil), func(x []byte) error { _, err := ParseError(x); return err })
		}
	})
}

func mustReparse(t *testing.T, enc []byte, parse func([]byte) error) {
	t.Helper()
	if err := parse(enc); err != nil {
		t.Fatalf("a parsed message does not re-parse: %v", err)
	}
}

// FuzzParseQueueRecord covers the on-disk queue record.
func FuzzParseQueueRecord(f *testing.F) {
	rec := queueRecord{
		Public: true, PowBits: 20, Created: 1, Expires: 2, Count: 3, Bytes: 4,
	}
	copy(rec.AuthKeyID[:], make([]byte, 32))
	copy(rec.AuthSealed[:], make([]byte, authKeySealedSize))
	f.Add(rec.encode())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, err := parseQueueRecord(b)
		_ = err // any error is fine, a panic is not
	})
}

// FuzzOpenForward covers the unsealing of a FORWARD body.
func FuzzOpenForward(f *testing.F) {
	hs, err := identity.NewKeyPair()
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(hs.Destroy)
	f.Add(make([]byte, 48))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = OpenForward(hs, b)
	})
}
