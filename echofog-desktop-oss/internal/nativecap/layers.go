package nativecap

// Temporal layers. With them the encoder codes base frames (layer 0) from base frames only, a
// middle layer from the base, and the top layer, every other frame, from either but never for
// another frame to refer to. A relay of the stream's tree then leaves the upper layers out for a
// child whose path cannot carry the whole stream: the child gets the base at a quarter or a half of
// the frame rate instead of losing packets of every frame (see the call's media layer).
//
// The NVIDIA encoder (and the Microsoft one) say each frame's layer in an SVC prefix NAL unit (type
// 14) before its slice; an encoder that takes the layer count but writes no prefix still marks the
// frames nothing refers to (nal_ref_idc 0), which can always be left out. The prefix units are
// taken out of the stream: a viewer's decoder has no use for them, the packet mark carries the layer.

// temporalLayersFor is how many layers a stream at fps asks the encoder for: three at 48 fps and
// more (a base of 12-30 fps), two from 20 fps, none below, where half the rate is too little.
func temporalLayersFor(fps int) int {
	switch {
	case fps >= 48:
		return 3
	case fps >= 20:
		return 2
	}
	return 1
}

// frameLayer is what a frame says of its layer.
type frameLayer struct {
	tid         uint8
	discardable bool // no slice of the frame is a reference
}

// splitLayer reads an access unit's layer and returns the unit without its SVC prefix NAL units.
// Without a prefix, a frame that is not a reference goes to layer 1, any other to layer 0.
func splitLayer(au []byte) ([]byte, frameLayer) {
	var l frameLayer
	prefix, slices, refs := false, 0, 0
	keep := make([]byte, 0, len(au))
	for _, n := range annexBNALs(au) {
		switch typ := n[0] & 0x1f; typ {
		case 14, 20: // prefix NAL, coded slice extension: the SVC part of the stream
			if typ == 14 && len(n) >= 4 && !prefix {
				prefix, l.tid = true, n[3]>>5
			}
			continue
		case 1, 5:
			slices++
			if n[0]&0x60 != 0 {
				refs++
			}
			if typ == 5 {
				l.tid = 0
				prefix = true // an IDR is the base whatever a prefix says
			}
		}
		keep = append(keep, 0, 0, 0, 1)
		keep = append(keep, n...)
	}
	l.discardable = slices > 0 && refs == 0
	if !prefix && l.discardable {
		l.tid = 1
	}
	return keep, l
}
