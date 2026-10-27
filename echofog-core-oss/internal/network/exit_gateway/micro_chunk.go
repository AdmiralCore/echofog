package exit_gateway

// micro_chunk: a sub-16KB chunking layer for the exit-tunnel TCP fallback data phase, to defeat
// the Russian TSPU "TCP freeze after ~16KB" behaviour (a single TCP socket to a foreign datacentre
// is throttled/frozen once its cumulative bytes cross ~16 KB).
//
// The tunnel data is carried as self-describing chunks tagged (TunnelID, Seq) and reassembled at
// the exit. A rotator seam lets the writer move to a fresh underlying stream before any one socket
// crosses the freeze counter, so the cumulative-per-socket stays under the threshold while the
// (TunnelID, Seq) framing lets the receiver stitch the chunks back together in order regardless of
// which socket each arrived on.
//
// Chunk on the wire (each direction independent):
//
//	[tunnelID:8 BE][seq:4 BE][flags:1][dataLen:2 BE][data: dataLen]
//	flags bit0 = FIN: the last chunk of this direction (it may still carry data).
//
// The layer is byte-transparent: the bytes delivered by Read equal the bytes passed to Write, in
// order, whatever the chunk boundaries or (under rotation / a test) the arrival order. It is gated
// exactly like the TLS-in-TLS padding (see padData / P2PDialer.chunk) and is off by default, so a
// node that does not opt in behaves exactly as before.
//
// This file ships the framing, reassembly, rotation seam and the stall detector. Live cross-peer
// negotiation (a handshake capability) and feeding real fresh sockets to the rotator both live in
// the swarm/session layer and are integrated separately; here the rotator is nil unless a caller
// (or a test) supplies one, in which case the layer is a correct, byte-transparent reframing of a
// single stream.

import (
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/config"
)

const (
	chunkHdrLen = 15 // [tunnelID:8][seq:4][flags:1][dataLen:2]

	// chunkMaxData is the most real data bytes carried per chunk. It sits in the requested 10-13 KB
	// band and fits the uint16 dataLen field.
	chunkMaxData = 12 * 1024 // 12288

	// socketByteCap is the cumulative-per-socket ceiling the writer keeps under. TSPU freezes a
	// socket past ~16 KB; one 12 KB chunk (plus header) fits under this cap, a second would cross it,
	// so the writer rotates to a fresh socket first and no socket ever carries ~16 KB.
	socketByteCap = 15 * 1024 // 15360

	// Reorder bounds: a receiver buffers at most this many out-of-order chunks / bytes before it
	// treats the stream as hostile (a peer replaying or lying about seq cannot make it allocate
	// without bound). Under honest single-stream delivery nothing is ever buffered.
	maxReorderChunks = 64
	maxReorderBytes  = 64 * (chunkMaxData + chunkHdrLen)

	chunkFlagFIN byte = 1 << 0
)

var (
	// ErrChunkProtocol: the peer sent a chunk that cannot belong to this tunnel (wrong TunnelID,
	// an over-long dataLen, or more reordering than the bounds allow).
	ErrChunkProtocol = errors.New("micro-chunk: protocol violation")
)

// rotator yields a fresh underlying stream for the writer to continue on, so the next chunks ride a
// new socket/5-tuple. A nil rotator means "no rotation": the writer stays on its first stream (the
// framing is still applied and is byte-transparent, it just does not defeat the per-socket freeze).
type rotator func() (FramedStream, error)

// MicroChunkEngaged is the gate: whether a tunnel's data phase should be carried in micro-chunks.
// It fires on the explicit config opt-in (AntiDPI.MicroChunkTCP) or when the region signal places
// the egress path in Russia — the same GeoIP/HomeCountry signal region_obfs.go already computes,
// passed in as pathInRU so this package does not re-reach into GeoIP. It is false whenever neither
// fires, so the default path (and the single-region testbed) is untouched. The node/swarm layer
// supplies pathInRU and wires the result into the dialer and exit service; that activation, and the
// cross-peer capability negotiation it needs, are integrated separately from this layer.
func MicroChunkEngaged(cfg config.AntiDPIConfig, pathInRU bool) bool {
	return cfg.MicroChunkTCP || pathInRU
}

// newTunnelID returns a random 64-bit tunnel identifier.
func newTunnelID() uint64 {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])
}

// chunkWriter frames an outbound byte stream into (TunnelID, Seq) chunks and rotates the underlying
// stream before any one of them crosses socketByteCap. One goroutine writes (the exit-tunnel data
// phase is single-writer per direction), so no locking is needed on the writer fields.
type chunkWriter struct {
	st       FramedStream // current underlying stream
	rotate   rotator      // nil: never rotate
	tunnelID uint64
	seq      uint32
	sockSent int // bytes written on the current underlying stream
	hdr      [chunkHdrLen]byte
	buf      []byte // header+data scratch, grown as needed
	finSent  bool
}

func newChunkWriter(st FramedStream, tunnelID uint64, rot rotator) *chunkWriter {
	return &chunkWriter{st: st, rotate: rot, tunnelID: tunnelID}
}

// Write frames p into chunks of at most chunkMaxData bytes and sends each as one underlying Write,
// rotating to a fresh stream before a chunk would push the current socket past socketByteCap. It
// reports len(p) written on success, as an io.Writer must, even though the wire carries more.
func (w *chunkWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > chunkMaxData {
			chunk = chunk[:chunkMaxData]
		}
		if err := w.writeChunk(chunk, false); err != nil {
			return total, err
		}
		total += len(chunk)
		p = p[len(chunk):]
	}
	return total, nil
}

// writeChunk emits one chunk, rotating first when it would not fit under the per-socket cap.
func (w *chunkWriter) writeChunk(data []byte, fin bool) error {
	need := chunkHdrLen + len(data)
	if w.sockSent > 0 && w.sockSent+need > socketByteCap {
		if err := w.doRotate(); err != nil {
			return err
		}
	}
	if cap(w.buf) < need {
		w.buf = make([]byte, need)
	}
	b := w.buf[:need]
	binary.BigEndian.PutUint64(b[0:8], w.tunnelID)
	binary.BigEndian.PutUint32(b[8:12], w.seq)
	if fin {
		b[12] = chunkFlagFIN
	} else {
		b[12] = 0
	}
	binary.BigEndian.PutUint16(b[13:15], uint16(len(data)))
	copy(b[chunkHdrLen:], data)
	if _, err := w.st.Write(b); err != nil {
		return err
	}
	w.seq++
	w.sockSent += need
	return nil
}

// doRotate swaps in a fresh underlying stream from the rotator and resets the per-socket counter.
// With no rotator it is a no-op: the writer keeps using its first stream.
func (w *chunkWriter) doRotate() error {
	if w.rotate == nil {
		return nil
	}
	st, err := w.rotate()
	if err != nil {
		return err
	}
	w.st = st
	w.sockSent = 0
	return nil
}

// CloseWrite emits a FIN chunk (so the receiver ends cleanly at a chunk boundary rather than on a
// bare transport EOF) and forwards the half-close to the underlying stream.
func (w *chunkWriter) CloseWrite() error {
	if !w.finSent {
		w.finSent = true
		if err := w.writeChunk(nil, true); err != nil {
			return err
		}
	}
	if cw, ok := w.st.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

// chunkReader reassembles a (TunnelID, Seq) chunk stream into the original byte stream, buffering
// out-of-order chunks and dropping duplicates. One goroutine reads per direction.
type chunkReader struct {
	src        chunkSource
	tunnelID   uint64
	haveID     bool
	nextWanted uint32

	cur     []byte // undelivered bytes of the current in-order chunk
	reorder map[uint32][]byte
	reBytes int
	fin     bool // the FIN chunk has been seen; EOF once everything up to it is delivered
	done    bool // FIN delivered: all further reads are EOF
	hdr     [chunkHdrLen]byte
}

// chunkSource supplies raw chunk-framed bytes. It is the underlying stream, abstracted so a test
// (or a future multi-socket exit) can feed chunks from more than one arriving stream into one
// reassembler.
type chunkSource interface {
	io.Reader
}

func newChunkReader(src chunkSource) *chunkReader {
	return &chunkReader{src: src, reorder: make(map[uint32][]byte)}
}

// newChunkReaderWithID pins the expected TunnelID up front (the exit learns it from the first
// chunk, but a caller that already knows it can reject a mismatching stream from the first byte).
func newChunkReaderWithID(src chunkSource, tunnelID uint64) *chunkReader {
	r := newChunkReader(src)
	r.tunnelID, r.haveID = tunnelID, true
	return r
}

// Read delivers reassembled bytes in order. It returns io.EOF once the FIN chunk and everything
// before it has been delivered; a stream that ends mid-chunk is io.ErrUnexpectedEOF.
func (r *chunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.cur) == 0 {
		if r.done {
			return 0, io.EOF
		}
		// Deliver a buffered in-order chunk before reading more from the wire.
		if data, ok := r.reorder[r.nextWanted]; ok {
			delete(r.reorder, r.nextWanted)
			r.reBytes -= len(data)
			r.advance(data)
			continue
		}
		if r.fin {
			// FIN seen and nothing buffered at nextWanted: everything is delivered.
			r.done = true
			return 0, io.EOF
		}
		if err := r.readChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	if len(r.cur) == 0 && r.done {
		return n, io.EOF
	}
	return n, nil
}

// advance makes data the current in-order chunk and bumps the sequence counter.
func (r *chunkReader) advance(data []byte) {
	r.cur = data
	r.nextWanted++
}

// readChunk reads one chunk header+data from the wire and either delivers it (in order), buffers it
// (ahead of order) or drops it (a duplicate already delivered).
func (r *chunkReader) readChunk() error {
	if _, err := io.ReadFull(r.src, r.hdr[:]); err != nil {
		// A truncated header is io.ErrUnexpectedEOF; io.EOF with nothing read is a clean end at a
		// chunk boundary (the peer closed the transport without a FIN chunk). Either propagates.
		return err
	}
	id := binary.BigEndian.Uint64(r.hdr[0:8])
	seq := binary.BigEndian.Uint32(r.hdr[8:12])
	flags := r.hdr[12]
	dataLen := int(binary.BigEndian.Uint16(r.hdr[13:15]))
	if dataLen > chunkMaxData {
		return ErrChunkProtocol
	}
	if !r.haveID {
		r.tunnelID, r.haveID = id, true
	} else if id != r.tunnelID {
		return ErrChunkProtocol
	}
	data := make([]byte, dataLen)
	if _, err := io.ReadFull(r.src, data); err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if flags&chunkFlagFIN != 0 {
		r.fin = true
	}
	switch {
	case seq == r.nextWanted:
		r.advance(data)
	case seq > r.nextWanted:
		if _, dup := r.reorder[seq]; !dup {
			if len(r.reorder) >= maxReorderChunks || r.reBytes+len(data) > maxReorderBytes {
				return ErrChunkProtocol
			}
			r.reorder[seq] = data
			r.reBytes += len(data)
		}
	default:
		// seq < nextWanted: a duplicate of an already-delivered chunk. Drop it.
	}
	return nil
}

// chunkedConn is the full-duplex layer wrapping one exit-tunnel data stream: it frames what is
// written and reassembles what is read. It stands in for the FramedStream it wraps; control frames
// (already exchanged before the data phase) pass straight through to the embedded stream. Reader
// and writer fields are disjoint and each is driven by its own goroutine, as the splicer does.
type chunkedConn struct {
	FramedStream
	w *chunkWriter
	r *chunkReader
}

// newChunkedConn wraps st for the client side, minting a fresh TunnelID. rot may be nil (no socket
// rotation; still byte-transparent).
func newChunkedConn(st FramedStream, rot rotator) *chunkedConn {
	id := newTunnelID()
	return &chunkedConn{
		FramedStream: st,
		w:            newChunkWriter(st, id, rot),
		r:            newChunkReader(st),
	}
}

// newChunkedConnExit wraps st for the exit side. The exit does not rotate (the client drives
// rotation); it learns the TunnelID from the first chunk it reads.
func newChunkedConnExit(st FramedStream) *chunkedConn {
	return &chunkedConn{
		FramedStream: st,
		w:            newChunkWriter(st, newTunnelID(), nil),
		r:            newChunkReader(st),
	}
}

func (c *chunkedConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *chunkedConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// CloseWrite emits the FIN chunk and forwards the half-close.
func (c *chunkedConn) CloseWrite() error { return c.w.CloseWrite() }

// ---- stall detector -------------------------------------------------------------------------

// freezeDetector decides whether a TCP exit tunnel has hit the TSPU freeze: it has carried at least
// freezeFloor bytes and then made no forward progress for stallTimeout. It is a small pure type so
// it can be driven by an injected clock in tests and reused both to decide to engage chunking on a
// path that is not chunked yet and as a safety-net rotation trigger once chunking is on.
type freezeDetector struct {
	mu           sync.Mutex
	floor        int64
	timeout      time.Duration
	total        int64
	lastProgress time.Time
	started      bool
}

// newFreezeDetector returns a detector; zero floor/timeout take the defaults (12 KB, 3s).
func newFreezeDetector(floor int64, timeout time.Duration) *freezeDetector {
	if floor <= 0 {
		floor = 12 * 1024
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &freezeDetector{floor: floor, timeout: timeout}
}

// progress records that n bytes moved at time now. Call it whenever the socket carries bytes either
// way; it resets the stall clock.
func (d *freezeDetector) progress(n int, now time.Time) {
	if n <= 0 {
		return
	}
	d.mu.Lock()
	d.total += int64(n)
	d.lastProgress = now
	d.started = true
	d.mu.Unlock()
}

// stalled reports whether the socket has crossed the freeze floor and then stopped making progress
// for at least the timeout: the signature of a TSPU freeze.
func (d *freezeDetector) stalled(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started || d.total < d.floor {
		return false
	}
	return now.Sub(d.lastProgress) >= d.timeout
}

// bytes returns the total bytes seen so far.
func (d *freezeDetector) bytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.total
}
