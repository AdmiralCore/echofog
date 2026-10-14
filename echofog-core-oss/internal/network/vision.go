package network

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"math/big"
)

// When the proxy carries a user's own HTTPS through our TCP/TLS tunnel, the inner TLS handshake
// has a telltale pattern of record sizes right at the start of the stream, which a censor can use
// to flag "TLS in TLS" even though everything is encrypted. visionStream blurs this the way XTLS
// Vision does: for the first visionPadBudget bytes of a stream it splits each write into chunks of
// random size and appends random padding, so the outer record sizes no longer mirror the inner
// handshake. After the budget it switches to a plain copy, so steady-state throughput is
// unaffected. Each direction counts its own real bytes, so the two ends stay in step without any
// negotiation.
const (
	visionPadBudget = 16 << 10 // real bytes per direction that are padded
	visionMaxChunk  = 1500     // split big early writes so records vary and stay small
	visionMaxPad    = 1024     // random padding added per frame
	visionHeaderLen = 4        // real length (uint16) + pad length (uint16)
)

// visionStream wraps a MuxStream, padding the start of the stream in both directions.
type visionStream struct {
	MuxStream
	w visionWriter
	r visionReader
}

type visionWriter struct {
	budget int // real bytes still to pad before switching to a plain copy
}

type visionReader struct {
	budget   int
	leftover []byte // real bytes of a frame not yet handed to the caller
	raw      bool
}

func newVisionStream(s MuxStream) MuxStream {
	return &visionStream{
		MuxStream: s,
		w:         visionWriter{budget: visionPadBudget},
		r:         visionReader{budget: visionPadBudget},
	}
}

func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

func (s *visionStream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 && s.w.budget > 0 {
		chunk := len(p)
		if chunk > visionMaxChunk {
			chunk = visionMaxChunk
		}
		if chunk > s.w.budget {
			chunk = s.w.budget
		}
		padLen := randInt(visionMaxPad + 1)
		frame := make([]byte, visionHeaderLen+chunk+padLen)
		binary.BigEndian.PutUint16(frame[0:2], uint16(chunk))
		binary.BigEndian.PutUint16(frame[2:4], uint16(padLen))
		copy(frame[visionHeaderLen:], p[:chunk])
		if padLen > 0 {
			_, _ = rand.Read(frame[visionHeaderLen+chunk:])
		}
		if _, err := s.MuxStream.Write(frame); err != nil {
			return written, err
		}
		written += chunk
		s.w.budget -= chunk
		p = p[chunk:]
	}
	if len(p) > 0 {
		n, err := s.MuxStream.Write(p)
		return written + n, err
	}
	return written, nil
}

func (s *visionStream) Read(p []byte) (int, error) {
	if len(s.r.leftover) > 0 {
		n := copy(p, s.r.leftover)
		s.r.leftover = s.r.leftover[n:]
		return n, nil
	}
	if s.r.raw {
		return s.MuxStream.Read(p)
	}
	var hdr [visionHeaderLen]byte
	if _, err := io.ReadFull(s.MuxStream, hdr[:]); err != nil {
		return 0, err
	}
	realLen := int(binary.BigEndian.Uint16(hdr[0:2]))
	padLen := int(binary.BigEndian.Uint16(hdr[2:4]))
	buf := make([]byte, realLen+padLen)
	if _, err := io.ReadFull(s.MuxStream, buf); err != nil {
		return 0, err
	}
	s.r.budget -= realLen
	if s.r.budget <= 0 {
		s.r.raw = true
	}
	real := buf[:realLen]
	n := copy(p, real)
	s.r.leftover = real[n:]
	return n, nil
}
