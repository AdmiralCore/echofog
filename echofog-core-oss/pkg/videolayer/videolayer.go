// Package videolayer is the mark a stream's video packets carry so that the relays of its tree can
// leave temporal layers out for a child whose path cannot carry them all.
//
// An encoder with temporal layers (L1T3: base frames at a quarter of the frame rate, a middle layer
// at half, the rest on top) codes every frame of a layer from frames of its own layer or below only.
// Without the layers above some layer, what is left still decodes, at a lower frame rate. The mark
// says which layer a packet's frame is in, where the frame starts, and how many packets of each
// layer the stream has sent up to it; a relay drops whole frames above a child's cap, and the
// child, from the counts alone, tells a frame left out on purpose from one lost on the way.
//
// It rides as an RTP header extension element (RFC 8285, one-byte form) inside the sealed unit, so
// only members see it. Viewers' bridges strip header extensions before the webview, and a node
// that does not know it takes the packet as any other: older viewers get the full stream as before.
package videolayer

import "encoding/binary"

const (
	// ExtensionID is the element's ID in the one-byte header extension.
	ExtensionID = 10
	// DataLen is the element's length.
	DataLen = 9
	// ExtensionLen is what the mark adds to an RTP packet that has no other extension: the
	// extension header, the element and its padding.
	ExtensionLen = 4 + (1+DataLen+3)/4*4

	profileOneByte = 0xBEDE

	// All is the cap that keeps every layer.
	All uint8 = 7

	// MaxFrameUnits is the most packets a frame's mark may say it has: far over a 4K keyframe's. A
	// mark past it, or one whose index is not below its count, is not taken (see Find).
	MaxFrameUnits = 4096
)

// Mark is what one packet says of its frame.
type Mark struct {
	TID         uint8  // the frame's temporal layer, 0 = base
	Start, End  bool   // the frame's first and last packet
	Key         bool   // an IDR frame: always kept
	Discardable bool   // no other frame refers to this one
	Index       uint16 // this packet's place in its frame, from 0
	Count       uint16 // packets the frame has
	// L0 and L1 count the packets of layer 0, and of layers 0 and 1, the stream sent up to this one
	// and with it (wrapping). The sender's core sets them as it numbers the units: a viewer that
	// gets layers up to c finds them consecutive in Layer(c), whatever else was left out.
	L0, L1 uint16
}

// Layer is the count of packets of layers up to c up to this one; seq stands for every layer
// (the RTP sequence number) from layer 2 on.
func (m Mark) Layer(c uint8, seq uint16) uint16 {
	switch c {
	case 0:
		return m.L0
	case 1:
		return m.L1
	}
	return seq
}

// Class is the step of the cap ladder a packet's layer is dropped at: 0, 1 or All.
func (m Mark) Class() uint8 {
	if m.TID >= 2 {
		return All
	}
	return m.TID
}

// Data encodes the element's data.
func (m Mark) Data() []byte {
	b := make([]byte, DataLen)
	m.put(b)
	return b
}

func (m Mark) put(b []byte) {
	f := m.TID & 0x07
	if m.Start {
		f |= 0x80
	}
	if m.End {
		f |= 0x40
	}
	if m.Key {
		f |= 0x20
	}
	if m.Discardable {
		f |= 0x10
	}
	b[0] = f
	binary.BigEndian.PutUint16(b[1:3], m.Index)
	binary.BigEndian.PutUint16(b[3:5], m.Count)
	binary.BigEndian.PutUint16(b[5:7], m.L0)
	binary.BigEndian.PutUint16(b[7:9], m.L1)
}

func parse(b []byte) Mark {
	return Mark{
		TID: b[0] & 0x07, Start: b[0]&0x80 != 0, End: b[0]&0x40 != 0, Key: b[0]&0x20 != 0, Discardable: b[0]&0x10 != 0,
		Index: binary.BigEndian.Uint16(b[1:3]), Count: binary.BigEndian.Uint16(b[3:5]),
		L0: binary.BigEndian.Uint16(b[5:7]), L1: binary.BigEndian.Uint16(b[7:9]),
	}
}

// sane reports whether a mark can be what a packetizer made: the packet's place lies within its
// frame, and the frame is not larger than any frame can be. A relay that does not check a unit's
// signature reads the mark all the same, so whatever it says must stay within these bounds.
func (m Mark) sane() bool { return m.Count > 0 && m.Count <= MaxFrameUnits && m.Index < m.Count }

// Find returns the mark of an RTP packet and the offset of its data in pkt; ok is false when the
// packet carries none (an encoder without layers, the webview's camera, an older sender) or one
// that is not sane: such a packet goes as an unmarked one.
func Find(pkt []byte) (m Mark, off int, ok bool) {
	off, ok = element(pkt)
	if !ok {
		return Mark{}, 0, false
	}
	if m = parse(pkt[off : off+DataLen]); !m.sane() {
		return Mark{}, 0, false
	}
	return m, off, true
}

// Strip returns pkt without an element that looks like the mark (ID ExtensionID, DataLen long),
// for a packet whose mark is not to be trusted: the webview's capture, which nothing of ours marks.
// The element becomes padding in a copy; a packet without one comes back as it is.
func Strip(pkt []byte) []byte {
	off, ok := element(pkt)
	if !ok {
		return pkt
	}
	out := append([]byte(nil), pkt...)
	clear(out[off-1 : off+DataLen])
	return out
}

// element finds the data of the mark's element in an RTP packet's one-byte header extension.
func element(pkt []byte) (off int, ok bool) {
	if len(pkt) < 12 || pkt[0]>>6 != 2 || pkt[0]&0x10 == 0 {
		return 0, false
	}
	p := 12 + 4*int(pkt[0]&0x0f)
	if len(pkt) < p+4 || binary.BigEndian.Uint16(pkt[p:p+2]) != profileOneByte {
		return 0, false
	}
	end := p + 4 + 4*int(binary.BigEndian.Uint16(pkt[p+2:p+4]))
	if end > len(pkt) {
		return 0, false
	}
	for i := p + 4; i < end; {
		id, n := pkt[i]>>4, int(pkt[i]&0x0f)+1
		switch {
		case pkt[i] == 0: // padding
			i++
			continue
		case id == 15:
			return 0, false
		}
		if i+1+n > end {
			return 0, false
		}
		if id == ExtensionID && n == DataLen {
			return i + 1, true
		}
		i += 1 + n
	}
	return 0, false
}

// SetCounts writes L0 and L1 into the mark at off (from Find), in place.
func SetCounts(pkt []byte, off int, l0, l1 uint16) {
	binary.BigEndian.PutUint16(pkt[off+5:off+7], l0)
	binary.BigEndian.PutUint16(pkt[off+7:off+9], l1)
}
