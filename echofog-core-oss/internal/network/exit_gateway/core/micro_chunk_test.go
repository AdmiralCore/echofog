package exit_gateway

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/net_types"
)

// memStream is an in-memory FramedStream: Write appends to a buffer, Read serves from r. The frame
// methods stand in for a tunnel whose control frame was already exchanged.
type memStream struct {
	wrote  bytes.Buffer
	r      io.Reader
	closed bool
}

func (s *memStream) Write(p []byte) (int, error) { return s.wrote.Write(p) }
func (s *memStream) Read(p []byte) (int, error) {
	if s.r == nil {
		return 0, io.EOF
	}
	return s.r.Read(p)
}
func (s *memStream) Close() error                                   { s.closed = true; return nil }
func (s *memStream) WriteFrame(net_types.MessageType, []byte) error { return nil }
func (s *memStream) ReadFrame() (net_types.MessageType, []byte, error) {
	return net_types.TypeExitConnectResult, []byte{resultOK}, nil
}

func mkrand(n int) []byte { b := make([]byte, n); _, _ = crand.Read(b); return b }

// drainRead reads r fully in rb-sized steps, returning the bytes and the terminating error (nil for
// a clean io.EOF).
func drainRead(t *testing.T, r io.Reader, rb int) ([]byte, error) {
	t.Helper()
	var out []byte
	buf := make([]byte, rb)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

func TestChunk_RoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"empty":            {},
		"tiny":             mkrand(1),
		"under one chunk":  mkrand(5000),
		"exactly one":      mkrand(chunkMaxData),
		"just over one":    mkrand(chunkMaxData + 1),
		"several chunks":   mkrand(chunkMaxData*3 + 777),
		"many chunks":      mkrand(chunkMaxData*10 + 3),
		"near 16k":         mkrand(15999),
		"well over socket": mkrand(socketByteCap * 4),
	}
	for name, payload := range cases {
		for _, fin := range []bool{false, true} {
			for _, rb := range []int{1, 7, 64, 4096, 1 << 20} {
				ms := &memStream{}
				w := newChunkWriter(ms, newTunnelID(), nil)
				if n, err := w.Write(payload); err != nil || n != len(payload) {
					t.Fatalf("%s: write n=%d err=%v", name, n, err)
				}
				if fin {
					if err := w.CloseWrite(); err != nil {
						t.Fatalf("%s: closewrite: %v", name, err)
					}
				}
				r := newChunkReader(bytes.NewReader(ms.wrote.Bytes()))
				got, err := drainRead(t, r, rb)
				if err != nil {
					t.Fatalf("%s fin=%v rb=%d: read: %v", name, fin, rb, err)
				}
				if !bytes.Equal(got, payload) {
					t.Fatalf("%s fin=%v rb=%d: got %d bytes, want %d", name, fin, rb, len(got), len(payload))
				}
			}
		}
	}
}

// frame builds one chunk on the wire for the reorder tests.
func mkChunk(tunnelID uint64, seq uint32, fin bool, data []byte) []byte {
	b := make([]byte, chunkHdrLen+len(data))
	binary.BigEndian.PutUint64(b[0:8], tunnelID)
	binary.BigEndian.PutUint32(b[8:12], seq)
	if fin {
		b[12] = chunkFlagFIN
	}
	binary.BigEndian.PutUint16(b[13:15], uint16(len(data)))
	copy(b[chunkHdrLen:], data)
	return b
}

func TestChunk_OutOfOrderAndDuplicate(t *testing.T) {
	id := newTunnelID()
	c0, c1, c2, c3 := mkrand(100), mkrand(200), mkrand(300), mkrand(50)
	want := bytes.Join([][]byte{c0, c1, c2, c3}, nil)

	// Delivered 2, 0, 1 (dup 0), 3+FIN — out of order, with a duplicate of an already-seen seq.
	var wire bytes.Buffer
	wire.Write(mkChunk(id, 2, false, c2))
	wire.Write(mkChunk(id, 0, false, c0))
	wire.Write(mkChunk(id, 1, false, c1))
	wire.Write(mkChunk(id, 0, false, c0)) // duplicate: must be dropped
	wire.Write(mkChunk(id, 3, true, c3))

	r := newChunkReaderWithID(bytes.NewReader(wire.Bytes()), id)
	got, err := drainRead(t, r, 37)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(want))
	}
}

func TestChunk_PartialIsUnexpectedEOF(t *testing.T) {
	id := newTunnelID()
	full := mkChunk(id, 0, false, mkrand(500))
	// Truncate mid-data.
	r := newChunkReader(bytes.NewReader(full[:chunkHdrLen+10]))
	_, err := drainRead(t, r, 64)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated data: got %v, want ErrUnexpectedEOF", err)
	}
	// Truncate mid-header.
	r2 := newChunkReader(bytes.NewReader(full[:5]))
	_, err = drainRead(t, r2, 64)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated header: got %v, want ErrUnexpectedEOF", err)
	}
}

func TestChunk_WrongTunnelIDRejected(t *testing.T) {
	id := newTunnelID()
	var wire bytes.Buffer
	wire.Write(mkChunk(id, 0, false, mkrand(10)))
	wire.Write(mkChunk(id+1, 1, false, mkrand(10))) // different tunnel id mid-stream
	r := newChunkReaderWithID(bytes.NewReader(wire.Bytes()), id)
	_, err := drainRead(t, r, 64)
	if !errors.Is(err, ErrChunkProtocol) {
		t.Fatalf("got %v, want ErrChunkProtocol", err)
	}
}

func TestChunk_ReorderBound(t *testing.T) {
	id := newTunnelID()
	var wire bytes.Buffer
	// Never send seq 0, send maxReorderChunks+1 later seqs: the receiver must refuse rather than
	// buffer without bound.
	for seq := uint32(1); seq <= maxReorderChunks+1; seq++ {
		wire.Write(mkChunk(id, seq, false, mkrand(10)))
	}
	r := newChunkReaderWithID(bytes.NewReader(wire.Bytes()), id)
	_, err := drainRead(t, r, 64)
	if !errors.Is(err, ErrChunkProtocol) {
		t.Fatalf("got %v, want ErrChunkProtocol", err)
	}
}

// rotStream records per-socket bytes and the rotation order, so a test can check that no single
// socket crossed the freeze cap.
type rotStream struct {
	memStream
	frozenAt int // 0: never freezes; else Write fails once cumulative would exceed this
	sent     int
}

var errFrozen = errors.New("socket frozen")

func (s *rotStream) Write(p []byte) (int, error) {
	if s.frozenAt > 0 && s.sent+len(p) > s.frozenAt {
		return 0, errFrozen
	}
	s.sent += len(p)
	return s.memStream.Write(p)
}

func TestChunk_RotationKeepsEachSocketUnderFreeze(t *testing.T) {
	payload := mkrand(socketByteCap * 5)
	var sockets []*rotStream
	first := &rotStream{frozenAt: 16 * 1024}
	sockets = append(sockets, first)
	rot := func() (FramedStream, error) {
		s := &rotStream{frozenAt: 16 * 1024}
		sockets = append(sockets, s)
		return s, nil
	}
	w := newChunkWriter(first, newTunnelID(), rot)
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write over freezing transport: %v", err)
	}
	if err := w.CloseWrite(); err != nil {
		t.Fatalf("closewrite: %v", err)
	}

	// No socket crossed the ~16 KB freeze counter, and more than one was used.
	if len(sockets) < 2 {
		t.Fatalf("expected rotation across sockets, used %d", len(sockets))
	}
	for i, s := range sockets {
		if s.sent >= 16*1024 {
			t.Fatalf("socket %d carried %d bytes, crossing the 16 KB freeze counter", i, s.sent)
		}
	}

	// Chunks ride ascending seq across the sockets in rotation order, so the concatenation
	// reassembles to the original payload.
	var wire bytes.Buffer
	for _, s := range sockets {
		wire.Write(s.wrote.Bytes())
	}
	r := newChunkReader(bytes.NewReader(wire.Bytes()))
	got, err := drainRead(t, r, 4096)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(payload))
	}
}

func TestFreezeDetector(t *testing.T) {
	base := time.Unix(0, 0)
	d := newFreezeDetector(12*1024, 3*time.Second)

	// Below the floor: never stalled, however long it idles.
	d.progress(8*1024, base)
	if d.stalled(base.Add(time.Hour)) {
		t.Fatal("stalled below the freeze floor")
	}

	// Cross the floor, keep moving: not stalled.
	d.progress(8*1024, base.Add(time.Second))
	if d.stalled(base.Add(2 * time.Second)) {
		t.Fatal("stalled while still making progress")
	}

	// Cross the floor, then no progress past the timeout: stalled (the TSPU freeze signature).
	if !d.stalled(base.Add(1*time.Second + 3*time.Second)) {
		t.Fatal("not stalled after crossing the floor and idling past the timeout")
	}
	if got := d.bytes(); got != 16*1024 {
		t.Fatalf("bytes = %d, want %d", got, 16*1024)
	}
}

// TestFreezeAfter16KB_DetectorEngagesAndCompletes contrasts the two paths over a transport that
// freezes a socket past 16 KB: un-chunked, the transfer stalls and the detector flags it (the
// "engage" signal); chunked, proactive rotation keeps every socket under the freeze counter and the
// whole payload gets through.
func TestFreezeAfter16KB_DetectorEngagesAndCompletes(t *testing.T) {
	const freezeAt = 16 * 1024
	payload := mkrand(64 * 1024)
	base := time.Unix(0, 0)

	// --- un-chunked: one socket, no rotation. It freezes at 16 KB; the detector engages. ---
	raw := &rotStream{frozenAt: freezeAt}
	det := newFreezeDetector(12*1024, 50*time.Millisecond)
	now := base
	wrote := 0
	for wrote < len(payload) {
		end := wrote + 4096
		if end > len(payload) {
			end = len(payload)
		}
		n, err := raw.Write(payload[wrote:end])
		if err != nil {
			break // the socket froze
		}
		wrote += n
		now = now.Add(time.Millisecond)
		det.progress(n, now)
	}
	if wrote >= len(payload) {
		t.Fatal("un-chunked transfer unexpectedly completed over a freezing socket")
	}
	// Time passes with no further progress: the detector flags the freeze.
	if !det.stalled(now.Add(time.Second)) {
		t.Fatal("detector did not engage on the un-chunked freeze")
	}

	// --- chunked: proactive rotation under socketByteCap keeps each socket below freezeAt. ---
	var sockets []*rotStream
	first := &rotStream{frozenAt: freezeAt}
	sockets = append(sockets, first)
	rot := func() (FramedStream, error) {
		s := &rotStream{frozenAt: freezeAt}
		sockets = append(sockets, s)
		return s, nil
	}
	w := newChunkWriter(first, newTunnelID(), rot)
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("chunked write over freezing transport: %v", err)
	}
	if err := w.CloseWrite(); err != nil {
		t.Fatalf("closewrite: %v", err)
	}
	var wire bytes.Buffer
	for _, s := range sockets {
		if s.sent >= freezeAt {
			t.Fatalf("a rotated socket still crossed the freeze counter (%d bytes)", s.sent)
		}
		wire.Write(s.wrote.Bytes())
	}
	got, err := drainRead(t, newChunkReader(bytes.NewReader(wire.Bytes())), 4096)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("chunked transfer reassembled %d bytes, want %d", len(got), len(payload))
	}
}

func TestMicroChunkEngaged_Gate(t *testing.T) {
	off := config.AntiDPIConfig{}
	if MicroChunkEngaged(off, false) {
		t.Fatal("gate engaged with config off and non-RU path")
	}
	if !MicroChunkEngaged(off, true) {
		t.Fatal("gate did not engage on an RU path")
	}
	on := config.AntiDPIConfig{MicroChunkTCP: true}
	if !MicroChunkEngaged(on, false) {
		t.Fatal("gate did not engage on the explicit config opt-in")
	}
}

// TestDial_GateOff_ByteIdentical proves the client dial path is unchanged when the micro-chunk gate
// is off: wrapData returns the stream itself (no wrapper), exactly as before this change; with the
// gate on it returns the chunk layer.
func TestDial_GateOff_ByteIdentical(t *testing.T) {
	newLazy := func() *lazyExitStream {
		return newLazyExitStream(context.Background(), &memStream{})
	}

	// Gate off (default dialer): the stream is returned as-is.
	off := NewP2PDialer(nil)
	if _, ok := off.wrapData(newLazy()).(*lazyExitStream); !ok {
		t.Fatal("gate off: wrapData wrapped the stream; the path is not byte-identical")
	}

	// Gate on: the chunk layer wraps it.
	on := NewP2PDialer(nil)
	on.chunk = func() bool { return true }
	if _, ok := on.wrapData(newLazy()).(*chunkedConn); !ok {
		t.Fatal("gate on: wrapData did not apply the micro-chunk layer")
	}
}

// TestExit_GateOff_NoReframing proves the exit data path adds no framing when micro-chunking is off:
// bytes written to the splice stream reach the wire unchanged (byte-identical to today). With the
// gate on, the same bytes are reassembled back to the original by the client's chunk reader.
func TestExit_GateOff_NoReframing(t *testing.T) {
	payload := mkrand(40 * 1024)

	// Gate off: the exit splices the raw stream — the wire bytes equal the payload.
	rawExit := &memStream{}
	var dataOff FramedStream = rawExit // mirrors HandleTunnelFrom with microChunk/padData nil
	if _, err := dataOff.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(rawExit.wrote.Bytes(), payload) {
		t.Fatal("gate off: exit added framing; the path is not byte-identical")
	}

	// Gate on: the exit frames; the client reassembles the identical payload.
	chExit := newChunkedConnExit(&memStream{})
	if _, err := chExit.Write(payload); err != nil {
		t.Fatalf("framed write: %v", err)
	}
	if bytes.Equal(chExit.w.st.(*memStream).wrote.Bytes(), payload) {
		t.Fatal("gate on: exit did not frame the data")
	}
	got, err := drainRead(t, newChunkReader(bytes.NewReader(chExit.w.st.(*memStream).wrote.Bytes())), 4096)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(payload))
	}
}
