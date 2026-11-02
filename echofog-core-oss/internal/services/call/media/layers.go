package media

import (
	"encoding/binary"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/videolayer"
)

// Temporal layers in the tree. A stream whose encoder makes them marks every packet with its
// frame's layer (videolayer). Each node that sends the stream on keeps a cap per child, set by the
// call layer from the child's path reports: frames of a layer above it are not sent to that child.
// Base frames (layer 0) and keyframes always go; frames are kept or left out whole, and a change
// of cap waits for the next base frame, from which the layers above it decode again.
//
// The child must tell a frame left out from one lost, and its webview must see packets numbered
// without holes. Both come from the mark's counts: a viewer that gets layers up to c finds the
// packets it gets numbered one after another in the count of layers up to c, so the holes in the
// unit numbers between two of them that the counts do not account for were left out, and its
// bridge gets the packets renumbered in that count. Which c a unit came with, the hop that sent it
// says in the fragment (HopCap): a child that reports it reads that is the only one whose layers
// are cut, and the only one sent the cap; any other child gets the whole stream we have, as before.
//
// FEC parities cover one layer class each (fecClasses), so a parity a child gets covers nothing
// that was left out for it: parities of a class above the child's cap are not sent either.

// cap ladder steps: the base, the base and the middle layer, everything
const (
	capBase uint8 = 0
	capMid  uint8 = 1
	capAll        = videolayer.All
)

// unitLayer is what a node knows of a video unit's layer.
type unitLayer struct {
	marked bool
	mark   videolayer.Mark
	frame  uint32 // the unit seq of the frame's first packet
	cap    uint8  // the cap the unit came here with: capAll when nothing cut it
}

// class is the ladder step the unit is left out at.
func (u unitLayer) class() uint8 {
	if !u.marked || u.mark.Key {
		return capBase
	}
	return u.mark.Class()
}

// layerOf reads the mark of a unit's plaintext (an RTP packet).
func layerOf(seq uint32, plain []byte) unitLayer {
	m, _, ok := videolayer.Find(plain)
	if !ok {
		return unitLayer{cap: capAll}
	}
	return unitLayer{marked: true, mark: m, frame: seq - uint32(m.Index), cap: capAll}
}

// ladderDown and ladderUp move a cap one step.
func ladderDown(c uint8) uint8 {
	switch c {
	case capAll:
		return capMid
	default:
		return capBase
	}
}

func ladderUp(c uint8) uint8 {
	if c == capBase {
		return capMid
	}
	return capAll
}

// childFilter is how one child of a stream gets its layers.
type childFilter struct {
	aware   bool  // the child reads HopCap: its layers may be cut
	want    uint8 // the cap the call layer set, taken at the next base frame
	cap     uint8 // the cap of the newest frame
	newest  uint32
	started bool
	frames  [32]frameCap // recent frames and the cap each was decided with
	next    int
	touched time.Time
}

type frameCap struct {
	start uint32
	cap   uint8
	used  bool
}

// active reports whether the child's layers are cut: it reads caps, or did and has not yet come
// back to every layer (a child that stops reporting gets them back from the next base frame on,
// not in the middle of frames that refer to layers it was not sent).
func (f *childFilter) active() bool { return f != nil && (f.aware || f.cap != capAll) }

// decide says whether a unit goes to the child, and the cap it goes with.
func (f *childFilter) decide(u unitLayer) (bool, uint8) {
	c := f.cap
	switch {
	case !f.started || int32(u.frame-f.newest) > 0:
		if u.mark.TID == 0 || u.mark.Key {
			f.cap = f.want
		}
		f.started, f.newest, c = true, u.frame, f.cap
		f.frames[f.next%len(f.frames)] = frameCap{u.frame, c, true}
		f.next++
	default:
		if fc, ok := f.capOf(u.frame); ok {
			c = fc
		}
	}
	return u.class() <= c, min(c, u.cap)
}

// capOf is the cap a recent frame was decided with.
func (f *childFilter) capOf(start uint32) (uint8, bool) {
	for _, fc := range f.frames {
		if fc.used && fc.start == start {
			return fc.cap, true
		}
	}
	return 0, false
}

// ringLayer is what the ring keeps of a unit's layer, to resend it only to whom it went.
type ringLayer struct {
	marked bool
	class  uint8
	frame  uint32
	cap    uint8
}

func (u unitLayer) ring() ringLayer {
	return ringLayer{marked: u.marked, class: u.class(), frame: u.frame, cap: u.cap}
}

// SetChildLayers sets how a child of a stream gets its layers: aware is whether it reads the cap
// (its report says so), maxCap the highest layer step it gets (videolayer.All: every layer). A cap
// that changes is taken at the next base frame.
func (d *MediaDispatcher) SetChildLayers(streamID string, child types.PeerID, aware bool, maxCap uint8) {
	if maxCap != capBase && maxCap != capMid {
		maxCap = capAll
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.filters == nil {
		d.filters = make(map[linkKey]*childFilter)
	}
	k := linkKey{streamID, child}
	f := d.filters[k]
	if f == nil {
		f = &childFilter{want: capAll, cap: capAll}
		d.filters[k] = f
	}
	f.aware, f.want, f.touched = aware, maxCap, time.Now()
	if !aware {
		f.want = capAll
	}
}

// ChildLayers returns the cap a child of a stream gets its frames with now.
func (d *MediaDispatcher) ChildLayers(streamID string, child types.PeerID) uint8 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if f := d.filters[linkKey{streamID, child}]; f.active() {
		return f.cap
	}
	return capAll
}

// StreamLayers returns the highest layer step a stream's units have been seen with lately (capBase
// for a stream without layers) and the cap this node gets it with.
func (d *MediaDispatcher) StreamLayers(streamID string) (top, got uint8) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t, ok := d.layerTop[streamID]
	if !ok || time.Since(t.at) > layerTopTTL {
		return capBase, capAll
	}
	got = capAll
	if c := d.caps[sourceKey{FrameVideo, t.broadcaster, streamID}]; c != nil && c.cur.set {
		got = c.cur.cap
	}
	return t.top, got
}

// layerTopTTL is how long a stream counts as layered after its last marked unit.
const layerTopTTL = 5 * time.Second

type layerTop struct {
	top         uint8
	at          time.Time
	broadcaster types.PeerID
}

// noteLayerLocked keeps the highest step seen of a stream; d.mu must be held.
func (d *MediaDispatcher) noteLayerLocked(streamID string, b types.PeerID, u unitLayer, now time.Time) {
	if !u.marked {
		return
	}
	if d.layerTop == nil {
		d.layerTop = make(map[string]layerTop)
	}
	t := d.layerTop[streamID]
	if now.Sub(t.at) > layerTopTTL || t.broadcaster != b {
		t.top = capBase
	}
	t.top, t.at, t.broadcaster = max(t.top, u.mark.Class()), now, b
	d.layerTop[streamID] = t
}

// regime is a cap a stream came with from some unit on.
type regime struct {
	cap  uint8
	from uint32
	off  uint16 // renumbering: RTP seq = count of layers up to cap + off
	set  bool
}

// capTrack is the caps a stream came with here: the current one and the one before it.
type capTrack struct {
	cur, prev regime
}

// at is the cap a unit with this seq came with, for a unit that says none of its own (rebuilt
// from FEC parity here).
func (c *capTrack) at(seq uint32) uint8 {
	if !c.cur.set {
		return capAll
	}
	if c.prev.set && int32(seq-c.cur.from) < 0 {
		return c.prev.cap
	}
	return c.cur.cap
}

// note takes the cap a unit came with.
func (c *capTrack) note(seq uint32, cp uint8) {
	switch {
	case !c.cur.set:
		c.cur = regime{cap: cp, from: seq, set: true}
	case cp == c.cur.cap:
	case c.prev.set && cp == c.prev.cap && int32(seq-c.cur.from) < 0:
		// a late unit from before the change
	default:
		c.prev, c.cur = c.cur, regime{cap: cp, from: seq, set: true}
	}
}

// arrivedCapLocked is the cap a unit came with: what its hop said, or for a unit rebuilt here,
// what the units around it came with. d.mu must be held.
func (d *MediaDispatcher) arrivedCapLocked(p *MediaPacket, u unitLayer, rebuilt bool) uint8 {
	if !u.marked {
		return capAll
	}
	k := sourceKey{FrameVideo, p.Broadcaster, p.StreamID}
	if d.caps == nil {
		d.caps = make(map[sourceKey]*capTrack)
	}
	c := d.caps[k]
	if c == nil {
		c = &capTrack{}
		d.caps[k] = c
	}
	if rebuilt {
		return c.at(p.Seq)
	}
	cp := uint8(capAll)
	if p.HopCap != 0 {
		cp = p.HopCap - 1
		if cp != capBase && cp != capMid {
			cp = capAll
		}
	}
	c.note(p.Seq, cp)
	return cp
}

// renumber writes into a delivered unit's RTP packet the sequence number the viewer's webview
// gets: the count of the layers it gets up to it, plus an offset that keeps the numbers going on
// one after another when the cap changes. A unit of the whole stream keeps its number until the
// first change. d.mu must be held.
func (d *MediaDispatcher) renumberLocked(k sourceKey, seq uint32, u unitLayer, plain []byte) {
	if !u.marked || len(plain) < 12 {
		return
	}
	if d.renum == nil {
		d.renum = make(map[sourceKey]*capTrack)
	}
	st := d.renum[k]
	if st == nil {
		st = &capTrack{}
		d.renum[k] = st
	}
	rtpSeq := binary.BigEndian.Uint16(plain[2:4])
	pos := func(c uint8) uint16 { return u.mark.Layer(c, rtpSeq) }
	var off uint16
	switch c := u.cap; {
	case !st.cur.set:
		st.cur = regime{cap: c, from: seq, off: rtpSeq - pos(c), set: true}
		off = st.cur.off
	case c == st.cur.cap:
		off = st.cur.off
	case st.prev.set && c == st.prev.cap && int32(seq-st.cur.from) < 0:
		off = st.prev.off
	default:
		// the new count takes over where the old one would have put this unit, one further when
		// the unit is of a layer the old cap left out (the base frame the change waited for was lost)
		old := pos(st.cur.cap) + st.cur.off
		if u.class() > st.cur.cap {
			old++
		}
		off = old - pos(c)
		st.prev, st.cur = st.cur, regime{cap: c, from: seq, off: off, set: true}
	}
	if out := pos(u.cap) + off; out != rtpSeq {
		binary.BigEndian.PutUint16(plain[2:4], out)
	}
}

// layerGap is the units between the last unit delivered (last, lastSeq) and the next one (u, seq)
// that should have come: all of them in a stream that is not cut, otherwise those of the two
// frames the units belong to, as long as the counts say nothing else is missing. expect is how many
// of the candidates should come: the gap is whole once that many did.
func layerGap(lastSeq uint32, last unitLayer, seq uint32, u unitLayer) (cands []uint32, expect int) {
	n := int(seq - lastSeq - 1)
	all := func() ([]uint32, int) {
		out := make([]uint32, 0, min(n, maxTrackedGap+1))
		for s := lastSeq + 1; s != seq && len(out) <= maxTrackedGap; s++ {
			out = append(out, s)
		}
		return out, n
	}
	if n <= 0 {
		return nil, 0
	}
	c := min(last.cap, u.cap)
	if !last.marked || !u.marked || c == capAll || u.frame == last.frame {
		return all()
	}
	m := int(uint16(u.mark.Layer(c, 0) - last.mark.Layer(c, 0)))
	if u.class() <= c {
		m--
	}
	if m <= 0 || m > n {
		if m == 0 {
			return nil, 0
		}
		return all()
	}
	tail, head := 0, int(u.mark.Index)
	if int(last.mark.Count) > int(last.mark.Index) {
		tail = int(last.mark.Count) - int(last.mark.Index) - 1
	}
	if tail+head > n || m != tail+head {
		out, _ := all()
		return out, m
	}
	for i := 1; i <= tail; i++ {
		cands = append(cands, lastSeq+uint32(i))
	}
	for i := head; i >= 1; i-- {
		cands = append(cands, seq-uint32(i))
	}
	return cands, m
}

// gapGroup is the units one gap waits for: whole once expect of them came.
type gapGroup struct {
	seqs   []uint32
	expect int
	got    int
}

// space is the numbering a delivered unit's path is measured in (feedback.go): the count of the
// layers it came with when they are cut, otherwise the unit seq (pos is then unused).
func (u unitLayer) space(seq uint32, plain []byte) (uint8, uint16) {
	if !u.marked || u.cap == capAll || len(plain) < 4 {
		return capAll, 0
	}
	return u.cap, u.mark.Layer(u.cap, binary.BigEndian.Uint16(plain[2:4]))
}

// gcLayersLocked forgets the layer state of children and streams gone quiet; d.mu must be held.
func (d *MediaDispatcher) gcLayersLocked(now time.Time) {
	for k, f := range d.filters {
		if now.Sub(f.touched) > 10*time.Second {
			delete(d.filters, k)
		}
	}
	live := func(stream string) bool {
		t, ok := d.layerTop[stream]
		return ok && now.Sub(t.at) < 30*time.Second
	}
	for k := range d.caps {
		if !live(k.streamID) {
			delete(d.caps, k)
		}
	}
	for k := range d.renum {
		if !live(k.streamID) {
			delete(d.renum, k)
		}
	}
	for k := range d.lastLayer {
		if !live(k.streamID) {
			delete(d.lastLayer, k)
		}
	}
	for id, t := range d.layerTop {
		if now.Sub(t.at) >= 30*time.Second {
			delete(d.layerTop, id)
		}
	}
}
