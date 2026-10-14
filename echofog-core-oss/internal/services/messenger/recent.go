package messenger

import (
	"encoding/binary"
	"sync"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// recentMessages remembers which message id the last few thousand ratchet headers carried. A
// sender whose receipt got lost sends the same ciphertext again; its key is used up, so it cannot
// be read twice, but the id is known and the receipt can go out again.
type recentMessages struct {
	mu    sync.Mutex
	max   int
	ids   map[string]string
	order []string
}

func newRecentMessages(max int) *recentMessages {
	return &recentMessages{max: max, ids: make(map[string]string, max)}
}

func recentKey(from types.PeerID, h *internal_pb.RatchetHeader) string {
	if h == nil {
		return ""
	}
	k := make([]byte, 0, 32+len(h.RatchetKey)+8)
	k = append(k, from[:]...)
	k = append(k, h.RatchetKey...)
	k = binary.BigEndian.AppendUint32(k, h.Pn)
	k = binary.BigEndian.AppendUint32(k, h.N)
	return string(k)
}

func (r *recentMessages) put(from types.PeerID, h *internal_pb.RatchetHeader, id string) {
	k := recentKey(from, h)
	if k == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.ids[k]; !ok {
		r.order = append(r.order, k)
		if len(r.order) > r.max {
			delete(r.ids, r.order[0])
			r.order = r.order[1:]
		}
	}
	r.ids[k] = id
}

func (r *recentMessages) get(from types.PeerID, h *internal_pb.RatchetHeader) (string, bool) {
	k := recentKey(from, h)
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.ids[k]
	return id, ok
}
