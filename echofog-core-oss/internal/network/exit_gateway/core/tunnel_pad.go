package exit_gateway

// tunnel_pad: a Vision-style padding wrapper for the exit-tunnel data phase. It breaks the
// "TLS-in-TLS" size/timing fingerprint (step 3 of the anti-DPI plan): when a client opens an HTTPS
// site through the tunnel, the browser's inner TLS 1.3 ClientHello is written alone and flushed as
// one lone ~550 B QUIC datagram after an idle gap, then the server's certificate burst, then a
// small Finished. testbed/censor.LPInnerHellos keys on exactly that shape.
//
// For a short opening budget per direction the data is carried in self-describing chunks with
// randomized padding, so the ClientHello carrier leaves the detector's size window (and stays
// inside one QUIC datagram, so quic-go does not split it into an odd pair that would show up in the
// size distribution). After the budget both directions switch to a raw byte pipe, so bulk transfer
// pays nothing.
//
// Chunk on the wire (each direction independent):
//
//	[cmd:1][dataLen:2 BE][padLen:2 BE][data: dataLen][pad: padLen]
//	cmd 0 = padded data; cmd 1 = padded data + final (switch to raw after this chunk)
//
// The receiver forwards data, discards pad, and on cmd 1 reads raw afterwards. The switch marker
// rides a real data chunk, never an empty frame from Close, so closing races nothing: a reader that
// hits io.EOF exactly at a chunk boundary ends cleanly, and an EOF mid-chunk is io.ErrUnexpectedEOF.
//
// Padding is negotiated at the peer level (handshake.CapTunnelPadding), not per stream, so a peer
// pair either pads every TypeExitConnect data phase between them or none: see dialLazy and
// HandleTunnelFrom.
//
// The sizes are the writer's own business: the reader takes every length from the chunk header
// and switches to raw on the final marker, whatever the budget. So each node pads with its own
// ranges (padProfile, derived from a secret of its own by SetPadSeed) and a budget drawn per
// stream: identical numbers on every node would themselves be a fingerprint.

import (
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"sync/atomic"
)

const (
	padHdrLen   = 5    // [cmd:1][dataLen:2][padLen:2]
	padMaxChunk = 4096 // max real data bytes per framed chunk (fits the uint16 dataLen field)
	padBudget   = 4096 // real bytes carried framed per direction before raw, before SetPadSeed

	// A chunk with at most padDangerHi data bytes would sit on the wire near the detector window
	// [hello+16, hello+320] ~= [533,837]; it is inflated to a randomized total in
	// [padTargetLo,padTargetHi]. That clears the window and, after QUIC's own ~40 B overhead, stays
	// inside one ~1200 B datagram (the apernet fork's Chrome-parrot initial size is 1250 B, and the
	// project treats 1200 B as the per-path budget). Larger chunks already sit above the window and
	// get only light jitter, and may legitimately span several datagrams like a real cert burst.
	padDangerHi = 850
	padTargetLo = 900
	padTargetHi = 1100
	padCeiling  = 1150 // hard cap on an inflated chunk's total size, so it never splits
	padJitterLo = 16
	padJitterHi = 64

	padCmdData  byte = 0
	padCmdFinal byte = 1

	// the bounds every node's own profile is drawn within
	padTargetMin   = 880 // clears the detector window (~837) with a margin
	padBudgetMin   = 3 << 10
	padBudgetMax   = 16 << 10
	padJitterFloor = 8
)

// padProfile is one node's padding ranges: small chunks are inflated to [targetLo, targetHi],
// larger ones get [jitterLo, jitterHi] of pad, and each stream pads its first [budgetLo, budgetHi]
// real bytes (one value drawn per stream).
type padProfile struct {
	targetLo, targetHi int
	jitterLo, jitterHi int
	budgetLo, budgetHi int
}

// defaultPadProfile is what every v0.2.0 node uses, and this one until SetPadSeed.
var defaultPadProfile = padProfile{
	targetLo: padTargetLo, targetHi: padTargetHi,
	jitterLo: padJitterLo, jitterHi: padJitterHi,
	budgetLo: padBudget, budgetHi: padBudget,
}

var padProf atomic.Pointer[padProfile]

func currentPadProfile() padProfile {
	if p := padProf.Load(); p != nil {
		return *p
	}
	return defaultPadProfile
}

// SetPadSeed gives this node its own padding ranges, derived from secret: stable across restarts
// (so a node does not drift between profiles), different from every other node's, and not
// computable by anyone without the secret. The process holds one node, so the profile is global.
func SetPadSeed(secret []byte) {
	p := padProfileFrom(secret)
	padProf.Store(&p)
}

func padProfileFrom(secret []byte) padProfile {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("echofog/tunnel-pad/v1"))
	b := m.Sum(nil)
	u := func(i int) int { return int(binary.BigEndian.Uint16(b[i:])) }
	var p padProfile
	p.targetLo = padTargetMin + u(0)%121                 // 880..1000
	p.targetHi = min(p.targetLo+60+u(2)%181, padCeiling) // up to 240 B wide, never past one datagram
	p.jitterLo = padJitterFloor + u(4)%25                // 8..32
	p.jitterHi = p.jitterLo + 16 + u(6)%81               // up to 96 B wide
	p.budgetLo = padBudgetMin + u(8)%(5<<10+1)           // 3..8 KiB
	p.budgetHi = min(p.budgetLo+1<<10+u(10)%(8<<10+1), padBudgetMax)
	return p
}

// padPool is a fixed buffer of random bytes the pad body is copied from, so padding costs no
// crypto/rand call per chunk. On the wire everything is under the outer QUIC AEAD anyway, so the
// pad only needs to not be a constant; this also keeps the wrapper honest if it is ever layered
// over an unencrypted transport.
var padPool [2048]byte

func init() { _, _ = crand.Read(padPool[:]) }

// tunnelPadConn wraps an exit-tunnel data stream. It implements FramedStream so it can stand in for
// the stream it wraps (the control frames pass straight through to the embedded stream, which has
// already exchanged them by the time the data phase and padding begin).
type tunnelPadConn struct {
	FramedStream
	prof padProfile

	// writer side (one goroutine): framed until wSent reaches budget, then raw
	budget    int
	wSent     int
	wRaw      bool
	wbuf      []byte
	padCursor int

	// reader side (another goroutine): disjoint from the writer fields above
	rRaw      bool
	rem       int  // data bytes left to deliver from the current chunk
	skip      int  // pad bytes left to discard after the current chunk's data
	finalSeen bool // the current/last chunk was cmd=final: go raw once it is drained
	hdr       [padHdrLen]byte
	scratch   [2048]byte
}

func newTunnelPad(st FramedStream) *tunnelPadConn {
	prof := currentPadProfile()
	return &tunnelPadConn{
		FramedStream: st,
		prof:         prof,
		budget:       int(cryptoRandInt64(int64(prof.budgetLo), int64(prof.budgetHi))),
	}
}

// CloseWrite forwards the half-close; it never emits a synthetic final marker (that would race
// Write and leak an empty packet). The peer's reader ends on the resulting io.EOF.
func (c *tunnelPadConn) CloseWrite() error {
	if cw, ok := c.FramedStream.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Write frames and pads the opening budget, then writes raw. A chunk that crosses the budget is
// marked final and the remainder of p is written raw in the same call. Each chunk goes out in one
// underlying Write so quic-go packetizes it as one datagram.
func (c *tunnelPadConn) Write(p []byte) (int, error) {
	if c.wRaw {
		return c.FramedStream.Write(p)
	}
	total := 0
	for len(p) > 0 && !c.wRaw {
		chunk := p
		if len(chunk) > padMaxChunk {
			chunk = chunk[:padMaxChunk]
		}
		c.wSent += len(chunk)
		final := c.wSent >= c.budget
		if err := c.writeChunk(chunk, final); err != nil {
			return total, err
		}
		total += len(chunk)
		p = p[len(chunk):]
		if final {
			c.wRaw = true
		}
	}
	if len(p) > 0 { // remainder after the final chunk (or we were already raw)
		n, err := c.FramedStream.Write(p)
		return total + n, err
	}
	return total, nil
}

func (c *tunnelPadConn) writeChunk(data []byte, final bool) error {
	padLen := c.choosePad(len(data))
	need := padHdrLen + len(data) + padLen
	if cap(c.wbuf) < need {
		c.wbuf = make([]byte, need)
	}
	b := c.wbuf[:need]
	if final {
		b[0] = padCmdFinal
	} else {
		b[0] = padCmdData
	}
	binary.BigEndian.PutUint16(b[1:3], uint16(len(data)))
	binary.BigEndian.PutUint16(b[3:5], uint16(padLen))
	copy(b[padHdrLen:], data)
	c.fillPad(b[padHdrLen+len(data):])
	_, err := c.FramedStream.Write(b)
	return err
}

// choosePad returns the pad length for a chunk of dataLen real bytes. Small chunks (the inner
// ClientHello, ServerHello, Finished) are inflated to a randomized total in the target band so
// they leave the detector window; larger chunks get only light jitter.
func (c *tunnelPadConn) choosePad(dataLen int) int {
	p := &c.prof
	if dataLen <= padDangerHi {
		target := int(cryptoRandInt64(int64(p.targetLo), int64(p.targetHi)))
		if target > padCeiling {
			target = padCeiling
		}
		pad := target - padHdrLen - dataLen
		if pad < p.jitterLo {
			pad = int(cryptoRandInt64(int64(p.jitterLo), int64(p.jitterHi)))
		}
		return pad
	}
	return int(cryptoRandInt64(int64(p.jitterLo), int64(p.jitterHi)))
}

func (c *tunnelPadConn) fillPad(dst []byte) {
	for len(dst) > 0 {
		if c.padCursor >= len(padPool) {
			c.padCursor = 0
		}
		n := copy(dst, padPool[c.padCursor:])
		c.padCursor += n
		dst = dst[n:]
	}
}

// Read strips padding and delivers the inner data. It switches to a raw pipe once it has drained
// the chunk marked final.
func (c *tunnelPadConn) Read(p []byte) (int, error) {
	if c.rRaw {
		return c.FramedStream.Read(p)
	}
	for c.rem == 0 {
		if c.skip > 0 {
			if err := c.discard(c.skip); err != nil {
				return 0, err
			}
			c.skip = 0
		}
		if c.finalSeen {
			c.rRaw = true
			return c.FramedStream.Read(p)
		}
		cmd, dataLen, padLen, err := c.readHeader()
		if err != nil {
			return 0, err
		}
		c.rem = dataLen
		c.skip = padLen
		if cmd == padCmdFinal {
			c.finalSeen = true
		}
		// a pad-only chunk (dataLen 0) loops: its pad is skipped, then the next header is read
	}
	want := len(p)
	if want > c.rem {
		want = c.rem
	}
	n, err := c.FramedStream.Read(p[:want])
	c.rem -= n
	if c.rem == 0 && c.skip > 0 {
		// data delivered; drop this chunk's pad. A short read here is a truncated chunk.
		if derr := c.discard(c.skip); derr != nil && (err == nil || err == io.EOF) {
			err = derr
		}
		c.skip = 0
		if c.finalSeen {
			c.rRaw = true
		}
	} else if err == io.EOF && c.rem > 0 {
		err = io.ErrUnexpectedEOF // the stream ended in the middle of the chunk's data
	}
	return n, err
}

// readHeader reads one 5-byte chunk header. io.EOF exactly at the boundary (nothing read) is a
// clean end and is returned as-is; a truncated header is io.ErrUnexpectedEOF.
func (c *tunnelPadConn) readHeader() (cmd byte, dataLen, padLen int, err error) {
	if _, err = io.ReadFull(c.FramedStream, c.hdr[:]); err != nil {
		return 0, 0, 0, err
	}
	cmd = c.hdr[0]
	dataLen = int(binary.BigEndian.Uint16(c.hdr[1:3]))
	padLen = int(binary.BigEndian.Uint16(c.hdr[3:5]))
	return cmd, dataLen, padLen, nil
}

// discard reads and drops n bytes (pad). A short read here is always unexpected (pad follows a
// header that promised it), so EOF becomes io.ErrUnexpectedEOF via io.ReadFull.
func (c *tunnelPadConn) discard(n int) error {
	for n > 0 {
		m := n
		if m > len(c.scratch) {
			m = len(c.scratch)
		}
		if _, err := io.ReadFull(c.FramedStream, c.scratch[:m]); err != nil {
			return err
		}
		n -= m
	}
	return nil
}
