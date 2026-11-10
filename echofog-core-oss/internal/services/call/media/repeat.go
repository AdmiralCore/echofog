package media

import (
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Sound goes twice: each unit again right after the next one of its source, 20 ms later, on every
// hop (the speaker's, the broadcaster's and each relay's), as Opus RED does inside the packet. It
// has no NACK (a resend would come too late to play), and the webview's Opus adds no FEC of its
// own, as its loopback to the bridge never loses; a lost datagram was a click in the sound or a
// gap in a word. The copy that arrives second is a duplicate the replay window drops; a copy whose
// original was lost plays a unit late, which the webview's jitter buffer puts back in order.
// The cost is twice the sound's bitrate: 64 kbit/s for a voice, 320 for a screen's stereo.

// repeatTTL drops the unit a source left behind once it went quiet.
const repeatTTL = time.Second

// repeatKey is a (peer, source) pair: the unit a source sent last to that peer.
type repeatKey struct {
	to  types.PeerID
	src sourceKey
}

// repeatUnit is the fragments of the last unit sent, and when.
type repeatUnit struct {
	frags [][]byte
	at    time.Time
}

// audioRepeat holds the last unit sent of each source to each peer.
type audioRepeat struct {
	mu   sync.Mutex
	last map[repeatKey]repeatUnit
}

// sendAudio sends a unit of sound to a peer, then the unit before it again.
func (d *MediaDispatcher) sendAudio(to types.PeerID, src sourceKey, t transport.Kind, frags [][]byte, now time.Time) {
	for _, f := range frags {
		d.outbox.Send(to, ClassAudio, t, f)
	}
	k := repeatKey{to, src}
	r := &d.repeat
	r.mu.Lock()
	if r.last == nil {
		r.last = make(map[repeatKey]repeatUnit)
	}
	prev := r.last[k]
	r.last[k] = repeatUnit{frags: frags, at: now}
	r.mu.Unlock()
	if now.Sub(prev.at) < repeatTTL {
		for _, f := range prev.frags {
			d.outbox.Send(to, ClassAudio, t, f)
		}
	}
}

// gc forgets the sources that went quiet.
func (r *audioRepeat) gc(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, u := range r.last {
		if now.Sub(u.at) > repeatTTL {
			delete(r.last, k)
		}
	}
}
