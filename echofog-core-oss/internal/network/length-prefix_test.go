package network

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"testing"
)

// hugeAnnounceReader serves a 4-byte header announcing announce bytes, then immediately EOFs
// without sending a single body byte.
type hugeAnnounceReader struct {
	header []byte
	off    int
}

func newHugeAnnounceReader(announce uint32) *hugeAnnounceReader {
	h := make([]byte, 4)
	binary.BigEndian.PutUint32(h, announce)
	return &hugeAnnounceReader{header: h}
}

func (r *hugeAnnounceReader) Read(p []byte) (int, error) {
	if r.off >= len(r.header) {
		return 0, io.EOF
	}
	n := copy(p, r.header[r.off:])
	r.off += n
	return n, nil
}

func TestReadLengthPrefixRoundTrip(t *testing.T) {
	for _, payload := range [][]byte{
		{},
		[]byte("hello"),
		bytes.Repeat([]byte{0xAB}, 1<<20), // 1 MiB: larger than readChunk, exercises growth
	} {
		var buf bytes.Buffer
		if err := WriteLengthPrefix(&buf, payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := ReadLengthPrefix(&buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(payload))
		}
	}
}

func TestReadLengthPrefixTooLarge(t *testing.T) {
	r := newHugeAnnounceReader(MaxFrameSize + 1)
	_, err := ReadLengthPrefix(r)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

// A peer announcing 60 MiB and then sending nothing must return an error without allocating the
// announced 60 MiB: memory must follow received data, not the announced length.
func TestReadLengthPrefixHugeAnnounceDoesNotAllocate(t *testing.T) {
	const announce = 60 << 20

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	r := newHugeAnnounceReader(announce)
	_, err := ReadLengthPrefix(r)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
	}

	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated >= announce {
		t.Fatalf("allocated %d bytes for a %d-byte announce that sent no body", allocated, announce)
	}
}

func TestReadFrameLimitRejectsOversizeHandshakeFrame(t *testing.T) {
	r := newHugeAnnounceReader(HandshakeMaxFrameSize + 1)
	_, _, err := ReadFrameLimit(r, HandshakeMaxFrameSize)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}
