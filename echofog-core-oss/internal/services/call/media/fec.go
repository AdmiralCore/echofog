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
// Fragments are dealt to fecLanes groups in turn, so neighbours land in different groups: paths
// lose packets in bursts, and a burst of up to fecLanes fragments then costs each group one, which
// it repairs. Groups are small in a keyframe, where one loss stalls the picture longest.

// FrameVideoFEC is the parity of a group of a stream's video fragments. Its seq is the group's first
// unit and its fragment index the lane it was filled in. Older nodes drop it as malformed, which
// only costs them the repair. (0x05 was a first layout, of consecutive units only; nothing sends it.)
const FrameVideoFEC FrameKind = 0x06

const (
	fecLanes         = 4                      // groups filled in turn
	fecGroupFrags    = 10                     // fragments a parity covers in ordinary frames
	fecKeyGroupFrags = 4                      // ... and in a keyframe
	fecGroupAge      = 100 * time.Millisecond // an open group older than this is sent as it is
	fecKeep          = time.Second            // fragments and parities kept for repairs
	fecMaxUnits      = 4096                   // units kept per stream
	fecMaxParities   = 256                    // parities kept per stream

	// fecFragHeader is what the XOR covers of a fragment besides its ciphertext: its unit's
	// timestamp and the ciphertext length. The rest of its header follows from the parity's.
	fecFragHeader = 4 + 2
	// fecMember is how a parity names a fragment: its unit's seq after the first, and its index
	// and its unit's fragment count, four bits each.
	fecMember = 2
	// fecReserve is what video fragments leave free so that a parity, its header and the largest
	// fragment it covers, still fits one datagram: firstSeq, member count, the members.
	fecReserve = 4 + 1 + fecGroupFrags*fecMember + fecFragHeader
)

var errBadFEC = errors.New("malformed FEC payload")

// fecRef names one fragment: the unit's seq, the fragment's index, the unit's fragment count.
type fecRef struct {
	seq   uint32
	idx   uint8
	count uint8
}

// fecParity is a parsed parity: the fragments it covers and their XOR.
type fecParity struct {
	members []fecRef
	blob    []byte
	at      time.Time
}

// fecFrag is one fragment of a unit as the XOR sees it.
type fecFrag struct {
	ts    uint32
	chunk []byte
}

// encodeFECPayload lays a parity out: [firstSeq:4][members:1][(seq-firstSeq):1, idx<<4|count-1]...
// [xor], the XOR being over [ts:4][len:2][ciphertext] of every fragment, zero-padded to the longest.
func encodeFECPayload(members []fecRef, frags []fecFrag) []byte {
	size := 0
	for _, f := range frags {
		size = max(size, len(f.chunk))
	}
	first := members[0].seq
	head := 4 + 1 + fecMember*len(members)
	out := make([]byte, head+fecFragHeader+size)
	binary.BigEndian.PutUint32(out[0:4], first)
	out[4] = byte(len(members))
	for i, m := range members {
		out[5+2*i] = byte(m.seq - first)
		out[6+2*i] = m.idx<<4 | (m.count - 1)
	}
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
	head := 5 + fecMember*n
	if n == 0 || n > fecGroupFrags || len(b) < head+fecFragHeader {
		return nil, errBadFEC
	}
	first := binary.BigEndian.Uint32(b[0:4])
	p := &fecParity{members: make([]fecRef, n), at: now}
	for i := range p.members {
		m := fecRef{seq: first + uint32(b[5+2*i]), idx: b[6+2*i] >> 4, count: b[6+2*i]&0x0f + 1}
		if m.idx >= m.count {
			return nil, errBadFEC
		}
		p.members[i] = m
	}
	p.blob = append([]byte(nil), b[head:]...)
	return p, nil
}

// fecGroup is a group a broadcaster is filling.
type fecGroup struct {
	members []fecRef
	frags   []fecFrag
	key     bool // holds part of a keyframe: closes at fecKeyGroupFrags
	start   time.Time
}

// fecParityOut is a parity a broadcaster has to send.
type fecParityOut struct {
	streamID string
	lane     uint8
	first    uint32
	payload  []byte
}

type fecLanesOf struct {
	next  int
	lanes [fecLanes]*fecGroup
}

// fecSender builds the parities of local streams.
type fecSender struct {
	mu      sync.Mutex
	streams map[string]*fecLanesOf
}

func newFECSender() *fecSender { return &fecSender{streams: map[string]*fecLanesOf{}} }

func (g *fecGroup) parity(streamID string, lane int) fecParityOut {
	return fecParityOut{streamID, uint8(lane), g.members[0].seq, encodeFECPayload(g.members, g.frags)}
}

// add takes the wire fragments of a new unit of a stream, and its plaintext RTP packet, and returns
// the parities to send now: those of the groups this unit filled, and of groups that ran old.
func (s *fecSender) add(streamID string, seq uint32, wire [][]byte, rtp []byte, now time.Time) []fecParityOut {
	frags := make([]fecFrag, 0, len(wire))
	for _, w := range wire {
		p, err := Decode(w)
		if err != nil {
			return nil
		}
		frags = append(frags, fecFrag{p.Timestamp, p.Payload})
	}
	key := rtpKeyframe(rtp)

	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streams[streamID]
	if st == nil {
		st = &fecLanesOf{}
		s.streams[streamID] = st
	}
	var out []fecParityOut
	for i, f := range frags {
		lane := st.next % fecLanes
		st.next++
		g := st.lanes[lane]
		// a member names its unit by one byte after the group's first
		if g != nil && (now.Sub(g.start) > fecGroupAge || seq-g.members[0].seq > 255) {
			out = append(out, g.parity(streamID, lane))
			g = nil
		}
		if g == nil {
			g = &fecGroup{start: now}
			st.lanes[lane] = g
		}
		g.members = append(g.members, fecRef{seq, uint8(i), uint8(len(frags))})
		g.frags = append(g.frags, f)
		g.key = g.key || key
		want := fecGroupFrags
		if g.key {
			want = fecKeyGroupFrags
		}
		if len(g.members) >= want {
			out = append(out, g.parity(streamID, lane))
			st.lanes[lane] = nil
		}
	}
	return out
}

// flush closes the groups left open longer than fecGroupAge (a stream that went quiet) and returns
// their parities.
func (s *fecSender) flush(now time.Time) []fecParityOut {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []fecParityOut
	for id, st := range s.streams {
		open := false
		for lane, g := range st.lanes {
			if g == nil {
				continue
			}
			if now.Sub(g.start) > fecGroupAge {
				out = append(out, g.parity(id, lane))
				st.lanes[lane] = nil
				continue
			}
			open = true
		}
		if !open {
			delete(s.streams, id)
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
		if !q.covers(p.Seq, p.FragIdx) {
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

func (q *fecParity) covers(seq uint32, idx uint8) bool {
	for _, m := range q.members {
		if m.seq == seq && m.idx == idx {
			return true
		}
	}
	return false
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
	var miss fecRef
	missing := 0
	for _, m := range q.members {
		u := s.units[m.seq]
		if u != nil && u.count != m.count {
			return nil, true // the parity and the fragments disagree: nothing to rebuild
		}
		if u == nil || u.frags[m.idx] == nil {
			missing++
			miss = m
		}
	}
	if missing != 1 {
		return nil, missing == 0
	}
	blob := append([]byte(nil), q.blob...)
	for _, m := range q.members {
		if u := s.units[m.seq]; u != nil && u.frags[m.idx] != nil {
			xorFrag(blob, *u.frags[m.idx])
		}
	}
	n := int(binary.BigEndian.Uint16(blob[4:6]))
	if n == 0 || fecFragHeader+n > len(blob) {
		return nil, true
	}
	return (&MediaPacket{
		Kind:        FrameVideo,
		Seq:         miss.seq,
		Timestamp:   binary.BigEndian.Uint32(blob[0:4]),
		Broadcaster: k.broadcaster,
		StreamID:    k.streamID,
		FragIdx:     miss.idx,
		FragCount:   miss.count,
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
