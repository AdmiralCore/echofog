package media

import (
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

// Forward error correction for stream video. A lost fragment used to cost a NACK round trip at
// best, and a keyframe at worst; a keyframe is a hundred fragments or more, so on a path that loses
// one packet in a hundred it rarely arrived whole either, and the picture froze until one did.
//
// The broadcaster follows every group of fragments with one parity packet: the XOR of the group's
// sealed fragments. Whoever misses exactly one fragment of a group, a viewer or a relay in the
// tree, rebuilds it from the others and the parity, at once and without asking anybody. Relays
// forward the parity like any fragment and never decrypt: the rebuilt fragment is the sealed one,
// and the viewer's decryption still checks it.
//
// Groups are small in a keyframe, where one loss stalls the picture longest, and larger elsewhere.

// FrameVideoFEC is the parity of a group of a stream's video fragments. Its seq is the group's first
// unit. Older nodes drop it as malformed, which only costs them the repair.
const FrameVideoFEC FrameKind = 0x05

const (
	fecGroupFrags    = 10                    // fragments a parity covers in ordinary frames
	fecKeyGroupFrags = 4                     // ... and in a keyframe
	fecMinGroupFrags = 3                     // a frame's end closes a group of at least this many
	fecMaxGroupUnits = 16                    // units one parity names
	fecGroupAge      = 50 * time.Millisecond // an open group older than this is sent as it is
	fecKeep          = time.Second           // fragments and parities kept for repairs
	fecMaxUnits      = 4096                  // units kept per stream
	fecMaxParities   = 256                   // parities kept per stream

	// fecFragHeader is what the XOR covers of a fragment besides its ciphertext: its unit's
	// timestamp and the ciphertext length. The rest of its header follows from the parity's.
	fecFragHeader = 4 + 2
	// fecReserve is what video fragments leave free so that a parity, its header and the largest
	// fragment it covers, still fits one datagram: firstSeq, unit count, a count per unit.
	fecReserve = 4 + 1 + fecMaxGroupUnits + fecFragHeader
)

var errBadFEC = errors.New("malformed FEC payload")

// fecParity is a parsed parity: the units it covers, how many fragments each has, and the XOR.
type fecParity struct {
	first  uint32
	counts []uint8
	blob   []byte
	at     time.Time
}

// fecFrag is one fragment of a unit as the XOR sees it.
type fecFrag struct {
	ts    uint32
	chunk []byte
}

// encodeFECPayload lays a parity out: [firstSeq:4][units:1][count per unit][xor], the XOR being
// over [ts:4][len:2][ciphertext] of every fragment, zero-padded to the longest.
func encodeFECPayload(first uint32, counts []uint8, frags []fecFrag) []byte {
	size := 0
	for _, f := range frags {
		size = max(size, len(f.chunk))
	}
	head := 4 + 1 + len(counts)
	out := make([]byte, head+fecFragHeader+size)
	binary.BigEndian.PutUint32(out[0:4], first)
	out[4] = byte(len(counts))
	copy(out[5:], counts)
	xorFrag(out[head:], frags...)
	return out
}

// xorFrag XORs fragments into blob as [ts:4][len:2][ciphertext].
func xorFrag(blob []byte, frags ...fecFrag) {
	for _, f := range frags {
		var h [fecFragHeader]byte
		binary.BigEndian.PutUint32(h[0:4], f.ts)
		binary.BigEndian.PutUint16(h[4:6], uint16(len(f.chunk)))
		for i, b := range h {
			blob[i] ^= b
		}
		for i, b := range f.chunk {
			if fecFragHeader+i < len(blob) {
				blob[fecFragHeader+i] ^= b
			}
		}
	}
}

func decodeFECPayload(b []byte, now time.Time) (*fecParity, error) {
	if len(b) < 5 {
		return nil, errBadFEC
	}
	n := int(b[4])
	if n == 0 || n > fecMaxGroupUnits || len(b) < 5+n+fecFragHeader {
		return nil, errBadFEC
	}
	p := &fecParity{first: binary.BigEndian.Uint32(b[0:4]), at: now}
	p.counts = append([]uint8(nil), b[5:5+n]...)
	for _, c := range p.counts {
		if c == 0 || c > MaxFragments {
			return nil, errBadFEC
		}
	}
	p.blob = append([]byte(nil), b[5+n:]...)
	return p, nil
}

// fecGroup is the group a broadcaster is filling for one stream.
type fecGroup struct {
	first  uint32
	counts []uint8
	frags  []fecFrag
	start  time.Time
}

func (g *fecGroup) parity(streamID string) fecParityOut {
	return fecParityOut{streamID, g.first, encodeFECPayload(g.first, g.counts, g.frags)}
}

// fecSender builds the parities of local streams.
type fecSender struct {
	mu     sync.Mutex
	groups map[string]*fecGroup
}

func newFECSender() *fecSender { return &fecSender{groups: map[string]*fecGroup{}} }

// fecParityOut is a parity a broadcaster has to send.
type fecParityOut struct {
	streamID string
	first    uint32
	payload  []byte
}

// add takes the wire fragments of a new unit of a stream, and its plaintext RTP packet, and returns
// the parities to send now: one for a group that ran old before this unit, one for the group this
// unit fills or ends.
func (s *fecSender) add(streamID string, seq uint32, wire [][]byte, rtp []byte, now time.Time) []fecParityOut {
	frags := make([]fecFrag, 0, len(wire))
	for _, w := range wire {
		p, err := Decode(w)
		if err != nil {
			return nil
		}
		frags = append(frags, fecFrag{p.Timestamp, p.Payload})
	}
	key, marker := rtpKeyframe(rtp), rtpMarker(rtp)

	s.mu.Lock()
	defer s.mu.Unlock()
	var out []fecParityOut
	g := s.groups[streamID]
	if g != nil && (now.Sub(g.start) > fecGroupAge || seq != g.first+uint32(len(g.counts))) {
		out = append(out, g.parity(streamID))
		g = nil
	}
	if g == nil {
		g = &fecGroup{first: seq, start: now}
		s.groups[streamID] = g
	}
	g.counts = append(g.counts, uint8(len(frags)))
	g.frags = append(g.frags, frags...)
	want := fecGroupFrags
	if key {
		want = fecKeyGroupFrags
	}
	if len(g.frags) >= want || len(g.counts) >= fecMaxGroupUnits || (marker && len(g.frags) >= fecMinGroupFrags) {
		out = append(out, g.parity(streamID))
		delete(s.groups, streamID)
	}
	return out
}

// flush closes the groups left open longer than fecGroupAge (a stream that went quiet after a
// small frame) and returns their parities.
func (s *fecSender) flush(now time.Time) []fecParityOut {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []fecParityOut
	for id, g := range s.groups {
		if now.Sub(g.start) > fecGroupAge {
			out = append(out, g.parity(id))
			delete(s.groups, id)
		}
	}
	return out
}

// fecUnit is what a receiver holds of one unit.
type fecUnit struct {
	count uint8
	frags [MaxFragments]*fecFrag
	at    time.Time
}

type fecStream struct {
	units    map[uint32]*fecUnit
	parities []*fecParity
}

// fecReceiver keeps recent fragments and parities per source, and rebuilds a lost fragment when a
// group misses exactly one.
type fecReceiver struct {
	mu      sync.Mutex
	streams map[sourceKey]*fecStream
}

func newFECReceiver() *fecReceiver { return &fecReceiver{streams: map[sourceKey]*fecStream{}} }

func (r *fecReceiver) streamLocked(k sourceKey) *fecStream {
	s := r.streams[k]
	if s == nil {
		s = &fecStream{units: map[uint32]*fecUnit{}}
		r.streams[k] = s
	}
	return s
}

// fragment records a video fragment and returns the wire fragments that waiting parities can now
// rebuild.
func (r *fecReceiver) fragment(p *MediaPacket, now time.Time) [][]byte {
	k := sourceKey{FrameVideo, p.Broadcaster, p.StreamID}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.streamLocked(k)
	u := s.units[p.Seq]
	if u == nil {
		if len(s.units) >= fecMaxUnits {
			s.expire(now, true)
		}
		u = &fecUnit{count: p.FragCount, at: now}
		s.units[p.Seq] = u
	}
	if u.count != p.FragCount {
		return nil
	}
	u.frags[p.FragIdx] = &fecFrag{p.Timestamp, append([]byte(nil), p.Payload...)}
	var out [][]byte
	for i := 0; i < len(s.parities); i++ {
		q := s.parities[i]
		if d := int32(p.Seq - q.first); d < 0 || int(d) >= len(q.counts) {
			continue
		}
		rebuilt, done := s.repair(k, q)
		if rebuilt != nil {
			out = append(out, rebuilt)
		}
		if done {
			s.parities = append(s.parities[:i], s.parities[i+1:]...)
			i--
		}
	}
	return out
}

// parity records a parity and returns the wire fragment it rebuilds, if its group misses exactly
// one; a parity whose group misses more waits for fragments to come.
func (r *fecReceiver) parity(p *MediaPacket, now time.Time) ([]byte, error) {
	q, err := decodeFECPayload(p.Payload, now)
	if err != nil {
		return nil, err
	}
	k := sourceKey{FrameVideo, p.Broadcaster, p.StreamID}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.streamLocked(k)
	rebuilt, done := s.repair(k, q)
	if !done {
		if len(s.parities) >= fecMaxParities {
			s.expire(now, true)
		}
		s.parities = append(s.parities, q)
	}
	return rebuilt, nil
}

// repair rebuilds the one fragment q's group misses. done is false while more than one is missing,
// so the parity is worth keeping.
func (s *fecStream) repair(k sourceKey, q *fecParity) (rebuilt []byte, done bool) {
	missSeq, missIdx, missing := uint32(0), uint8(0), 0
	for i, c := range q.counts {
		seq := q.first + uint32(i)
		u := s.units[seq]
		if u != nil && u.count != c {
			return nil, true // the parity and the fragments disagree: nothing to rebuild
		}
		for f := uint8(0); f < c; f++ {
			if u == nil || u.frags[f] == nil {
				missing++
				missSeq, missIdx = seq, f
			}
		}
	}
	if missing != 1 {
		return nil, missing == 0
	}
	blob := append([]byte(nil), q.blob...)
	for i, c := range q.counts {
		if u := s.units[q.first+uint32(i)]; u != nil {
			for f := uint8(0); f < c; f++ {
				if u.frags[f] != nil {
					xorFrag(blob, *u.frags[f])
				}
			}
		}
	}
	n := int(binary.BigEndian.Uint16(blob[4:6]))
	if n == 0 || fecFragHeader+n > len(blob) {
		return nil, true
	}
	return (&MediaPacket{
		Kind:        FrameVideo,
		Seq:         missSeq,
		Timestamp:   binary.BigEndian.Uint32(blob[0:4]),
		Broadcaster: k.broadcaster,
		StreamID:    k.streamID,
		FragIdx:     missIdx,
		FragCount:   q.counts[missSeq-q.first],
		Payload:     blob[fecFragHeader : fecFragHeader+n],
	}).Encode(), true
}

// expire drops what is older than fecKeep; with hard set, everything older than half of it, to
// make room when a stream sends more than the caps hold.
func (s *fecStream) expire(now time.Time, hard bool) {
	cut := now.Add(-fecKeep)
	if hard {
		cut = now.Add(-fecKeep / 2)
	}
	for seq, u := range s.units {
		if u.at.Before(cut) {
			delete(s.units, seq)
		}
	}
	kept := s.parities[:0]
	for _, p := range s.parities {
		if !p.at.Before(cut) {
			kept = append(kept, p)
		}
	}
	s.parities = kept
}

// gc drops old fragments and parities, and streams that went silent.
func (r *fecReceiver) gc(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, s := range r.streams {
		s.expire(now, false)
		if len(s.units) == 0 && len(s.parities) == 0 {
			delete(r.streams, k)
		}
	}
}

// rtpPayload is the payload of an RTP packet, past its header, CSRCs and extension; nil when the
// packet is too short to be one.
func rtpPayload(b []byte) []byte {
	if len(b) < 12 || b[0]>>6 != 2 {
		return nil
	}
	off := 12 + 4*int(b[0]&0x0f)
	if b[0]&0x10 != 0 { // extension
		if len(b) < off+4 {
			return nil
		}
		off += 4 + 4*int(binary.BigEndian.Uint16(b[off+2:off+4]))
	}
	if len(b) <= off {
		return nil
	}
	return b[off:]
}

// rtpMarker reports the RTP marker bit: the last packet of a video frame.
func rtpMarker(b []byte) bool { return len(b) >= 12 && b[1]&0x80 != 0 }

// rtpKeyframe reports whether an H.264 RTP packet carries part of a keyframe: an IDR slice, or the
// SPS or PPS that come with one (single NAL unit, STAP-A or FU-A).
func rtpKeyframe(b []byte) bool {
	p := rtpPayload(b)
	if len(p) == 0 {
		return false
	}
	isKey := func(t byte) bool { return t == 5 || t == 7 || t == 8 }
	switch t := p[0] & 0x1f; t {
	case 24: // STAP-A: [hdr][size:2][nal]...
		for off := 1; off+3 <= len(p); {
			n := int(binary.BigEndian.Uint16(p[off : off+2]))
			if n == 0 || off+2+n > len(p) {
				break
			}
			if isKey(p[off+2] & 0x1f) {
				return true
			}
			off += 2 + n
		}
		return false
	case 28: // FU-A: [indicator][fu header]
		return len(p) >= 2 && isKey(p[1]&0x1f)
	default:
		return isKey(t)
	}
}

// rtpKeyframeEnd reports whether an H.264 RTP packet ends a keyframe: the last packet of a frame
// whose own payload is part of one.
func rtpKeyframeEnd(b []byte) bool { return rtpMarker(b) && rtpKeyframe(b) }
