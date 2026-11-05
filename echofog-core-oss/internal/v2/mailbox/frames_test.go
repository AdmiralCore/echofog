package mailbox

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	q := bytes.Repeat([]byte{0x90}, QueueSize)
	auth := bytes.Repeat([]byte{0xa1}, 64)
	cases := []struct {
		name  string
		typ   uint64
		enc   []byte
		again func([]byte) ([]byte, error)
	}{
		{"queue-create", FrameQueueCreate,
			QueueCreate{Req: 7, Queue: q, AuthKey: auth, TTL: 3600, Public: true, PowBits: 20}.Append(nil),
			func(b []byte) ([]byte, error) { m, err := ParseQueueCreate(b); return m.Append(nil), err }},
		{"queue-ok", FrameQueueOK, QueueOK{Req: 7, Expires: 123456}.Append(nil),
			func(b []byte) ([]byte, error) { m, err := ParseQueueOK(b); return m.Append(nil), err }},
		{"deposit", FrameDeposit, Deposit{Req: 8, Queue: q, Blob: []byte("hello"), Pow: 1403}.Append(nil),
			func(b []byte) ([]byte, error) { m, err := ParseDeposit(b); return m.Append(nil), err }},
		{"deposited", FrameDeposited, Deposited{Req: 8, Status: StatusFull, Item: []byte{1, 2}}.Append(nil),
			func(b []byte) ([]byte, error) { m, err := ParseDeposited(b); return m.Append(nil), err }},
		{"fetch", FrameFetch, Fetch{Req: 9, Queue: q, After: []byte{1}, Max: 10, Wait: 60, Auth: auth}.Append(nil),
			func(b []byte) ([]byte, error) { m, err := ParseFetch(b); return m.Append(nil), err }},
		{"items", FrameItems, Items{Req: 9, Items: []Item{{ID: []byte{1, 2}, Blob: []byte("x"), Stored: 99}}, More: true}.Append(nil),
			func(b []byte) ([]byte, error) { m, err := ParseItems(b); return m.Append(nil), err }},
		{"ack", FrameAck, Ack{Req: 10, Queue: q, IDs: [][]byte{{1}, {2}}, Auth: auth}.Append(nil),
			func(b []byte) ([]byte, error) { m, err := ParseAck(b); return m.Append(nil), err }},
		{"forward", FrameForward, Forward{Req: 11, Relay: q, RelayCard: []byte("card"), Sealed: bytes.Repeat([]byte{3}, 48)}.Append(nil),
			func(b []byte) ([]byte, error) { m, err := ParseForward(b); return m.Append(nil), err }},
		{"error", FrameError, Error{Req: 12, Code: CodePowInvalid}.Append(nil),
			func(b []byte) ([]byte, error) { m, err := ParseError(b); return m.Append(nil), err }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			again, err := tc.again(tc.enc)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !bytes.Equal(again, tc.enc) {
				t.Fatalf("re-encode %x, want %x", again, tc.enc)
			}
			if !frameTypeKnown(tc.typ) {
				t.Fatalf("frame type 0x%x is not in the service table", tc.typ)
			}
		})
	}
}

func TestFrameUnknownFieldsAreSkipped(t *testing.T) {
	raw := Deposit{Req: 4, Queue: bytes.Repeat([]byte{1}, QueueSize), Blob: []byte("b")}.Append(nil)
	// field 99, varint
	raw = append(raw, 0x98, 0x06, 0x2a)
	m, err := ParseDeposit(raw)
	if err != nil || m.Req != 4 || !bytes.Equal(m.Blob, []byte("b")) {
		t.Fatalf("unknown field broke parsing: %+v %v", m, err)
	}
}

func TestFrameRejectsWrongWireType(t *testing.T) {
	// DEPOSIT.queue (field 2) as a varint instead of bytes.
	raw := []byte{0x10, 0x01}
	if _, err := ParseDeposit(raw); err == nil {
		t.Fatal("a varint queue was accepted")
	}
	// FETCH.max (field 4) as bytes.
	raw = []byte{0x22, 0x01, 0x00}
	if _, err := ParseFetch(raw); err == nil {
		t.Fatal("a bytes max was accepted")
	}
}

func TestFrameRejectsOversizedFields(t *testing.T) {
	// A 65537-byte blob is outside DEPOSIT's stated blob limit; the frame limiter bounds the
	// payload, the parser bounds the field.
	blob := bytes.Repeat([]byte{9}, MaxBlob+1)
	raw := append([]byte{0x1a}, binary.AppendUvarint(nil, uint64(len(blob)))...)
	raw = append(raw, blob...)
	if _, err := ParseDeposit(raw); err == nil {
		t.Fatal("an oversized blob was accepted")
	}
	// ACK with more than 100 ids.
	ack := Ack{Req: 1, Queue: bytes.Repeat([]byte{1}, QueueSize)}
	for i := 0; i < MaxAckIDs+1; i++ {
		ack.IDs = append(ack.IDs, []byte{byte(i)})
	}
	if _, err := ParseAck(ack.Append(nil)); err == nil {
		t.Fatal("an ACK with too many ids was accepted")
	}
}

func TestFrameLimitsMatchService(t *testing.T) {
	max := map[uint64]int{
		FrameQueueCreate: 128, FrameQueueOK: 32, FrameDeposit: 65664, FrameDeposited: 48,
		FrameFetch: 160, FrameItems: 1048576, FrameAck: 2048, FrameForward: 73728, FrameError: 32,
	}
	for typ, want := range max {
		if got := FrameMax(typ); got != want {
			t.Fatalf("frame 0x%x max %d, want %d", typ, got, want)
		}
	}
}

func TestAuthInputLayout(t *testing.T) {
	sid := [16]byte{1, 2, 3}
	queue := bytes.Repeat([]byte{0x90}, QueueSize)
	got := AuthInput(OpAck, sid, queue, 0x0102030405060708)
	want := append([]byte(AuthLabel), sid[:]...)
	want = append(want, OpAck)
	want = append(want, queue...)
	want = binary.BigEndian.AppendUint64(want, 0x0102030405060708)
	if !bytes.Equal(got, want) {
		t.Fatalf("auth input %x, want %x", got, want)
	}
}

func TestPowBits(t *testing.T) {
	queue := bytes.Repeat([]byte{0x90}, QueueSize)
	blob := []byte("payload")
	for pow := uint64(0); pow < 1000; pow++ {
		h := PowHash(queue, blob, pow)
		if LeadingZeroBits(h[:]) >= 8 {
			if !ValidPow(queue, blob, pow, 8) {
				t.Fatal("a hash with 8 leading zero bits was rejected")
			}
			if ValidPow(queue, blob, pow, 9) {
				t.Fatal("a hash below the requested bits was accepted")
			}
			return
		}
	}
	t.Fatal("no 8-bit proof found in 1000 nonces")
}

func TestPowBitsCountsFromTheTop(t *testing.T) {
	if got := LeadingZeroBits([]byte{0x00, 0x7f}); got != 9 {
		t.Fatalf("%d, want 9", got)
	}
	if got := LeadingZeroBits([]byte{0xff}); got != 0 {
		t.Fatalf("%d, want 0", got)
	}
	if got := LeadingZeroBits(nil); got != 0 {
		t.Fatalf("%d, want 0", got)
	}
}

func TestConstants(t *testing.T) {
	if QueueSize != 16 || ItemSize != 16 {
		t.Fatal("queue and item ids are 16 bytes")
	}
	if MaxBlob != 65536 {
		t.Fatalf("MaxBlob %d", MaxBlob)
	}
	if DefaultPowBits != 20 || MaxPowBits != 26 {
		t.Fatalf("pow bits %d/%d", DefaultPowBits, MaxPowBits)
	}
	if MaxWait != 60*time.Second {
		t.Fatalf("MaxWait %v", MaxWait)
	}
	if CodePowInvalid != 0x100 {
		t.Fatalf("CodePowInvalid 0x%x", CodePowInvalid)
	}
}

// helpers ------------------------------------------------------------------------------------

func frameTypeKnown(typ uint64) bool {
	switch typ {
	case FrameQueueCreate, FrameQueueOK, FrameDeposit, FrameDeposited, FrameFetch,
		FrameItems, FrameAck, FrameForward, FrameError:
		return true
	}
	return false
}
