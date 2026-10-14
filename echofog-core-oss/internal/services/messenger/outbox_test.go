package messenger

import (
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"sort"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
)

// What was queued first is sent first, whatever the random ids are: group changes depend on it.
func TestOutboxKeysSortByTime(t *testing.T) {
	var peer types.PeerID
	peer[0] = 7
	ids := []string{"zzz", "aaa", "mmm"}
	var keys []string
	for i, id := range ids {
		keys = append(keys, string(outboxKey(outboxFolder(nil, peer), int64(1000+i), id)))
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	assert.Equal(t, keys, sorted)
	assert.Contains(t, keys[0], "/")
}

// A repeated message is recognised by its header and the oldest headers are forgotten.
func TestRecentMessagesConfirmRepeats(t *testing.T) {
	r := newRecentMessages(2)
	var a, b types.PeerID
	a[0], b[0] = 1, 2
	h := func(n uint32) *internal_pb.RatchetHeader {
		return &internal_pb.RatchetHeader{RatchetKey: []byte{9}, N: n}
	}
	r.put(a, h(1), "m1")
	id, ok := r.get(a, h(1))
	assert.True(t, ok)
	assert.Equal(t, "m1", id)
	_, ok = r.get(b, h(1))
	assert.False(t, ok, "another sender's header must not match")
	r.put(a, h(2), "m2")
	r.put(a, h(3), "m3")
	_, ok = r.get(a, h(1))
	assert.False(t, ok, "the oldest entry is forgotten")
}
