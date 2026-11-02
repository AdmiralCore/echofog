package call

import (
	"bytes"
	"crypto/rand"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wire records the call signals that cross the mesh, as a relay circuit on the way would see them.
type wire struct {
	mu     sync.Mutex
	frames []wireFrame
}

type wireFrame struct {
	from, to types.PeerID
	data     []byte
}

func (w *wire) tap(from, to types.PeerID, t net_types.MessageType, payload []byte) {
	if t != net_types.TypeCallSignaling && t != net_types.TypeCallSignalingLossy {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.frames = append(w.frames, wireFrame{from, to, append([]byte(nil), payload...)})
}

func (w *wire) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.frames = nil
}

func (w *wire) snapshot() []wireFrame {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]wireFrame(nil), w.frames...)
}

// Signals used to cross relay circuits as readable JSON: the call's ID, its trees, who speaks when.
// Between members that both announced a signal key they go sealed now, and the room works on them
// exactly as before; a member of an older build, which announced none, still gets them plain and
// still takes part.
func TestRoom_SignalsAreSealedBetweenNewerNodes(t *testing.T) {
	m := newMesh()
	w := &wire{}
	m.tap = w.tap
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	nodes := make([]*testNode, 4)
	for i := range nodes {
		s := newTestSigner()
		cm := newCallManager(meshPort{m, s.id()}, s, s.id())
		log := &mediaLog{units: make(map[string]map[uint32]bool)}
		cm.OnMedia = log.tap
		nodes[i] = &testNode{cm: cm, id: s.id(), log: log}
		m.mu.Lock()
		m.nodes[s.id()] = cm
		m.mu.Unlock()
	}
	older := nodes[3]
	older.cm.olderNode = true
	for _, nd := range nodes {
		require.NoError(t, nd.cm.JoinRoom("room-1", nodes[0].id, key, tree.PeerNodeProfile{UploadKbps: 40000, MaxRelayChildren: 2}))
	}
	t.Cleanup(func() {
		for _, nd := range nodes {
			_ = nd.cm.LeaveRoom()
		}
	})
	require.Eventually(t, func() bool { return roomConverged(nodes, nil) }, 5*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond) // the manifest with everybody's signal key reaches everybody
	w.reset()

	// a speaker's VAD state is flooded over the voice tree, through the older node too
	for _, nd := range nodes[:3] {
		nd.cm.mu.Lock()
		nd.cm.floodLocked(nd.cm.activeRoom, "vad_state", map[string]any{"speaker": nd.id, "speaking": true, "audio_db": -20})
		nd.cm.unlockAndFlush()
	}
	before := nodes[2].log.count(media.FrameAudio, nodes[1].id, "voice")
	require.NoError(t, nodes[1].cm.dispatcher().BroadcastLocalAudio([]byte("still works")))
	require.Eventually(t, func() bool {
		return nodes[2].log.count(media.FrameAudio, nodes[1].id, "voice") > before
	}, 5*time.Second, 20*time.Millisecond)
	time.Sleep(memberHeartbeatInterval + 500*time.Millisecond) // heartbeats, both ways

	frames := w.snapshot()
	sealed, plain := 0, 0
	for _, f := range frames {
		withOlder := f.from == older.id || f.to == older.id
		if withOlder {
			assert.False(t, isSealedSignal(f.data), "an older node gets plain signals and sends them")
			plain++
			continue
		}
		assert.True(t, isSealedSignal(f.data), "a signal between newer nodes crossed the wire plain: %.80s", f.data)
		assert.False(t, bytes.Contains(f.data, []byte("room-1")), "the call's ID is not readable on the way")
		assert.False(t, bytes.Contains(f.data, []byte("vad_state")), "nor who speaks")
		sealed++
	}
	assert.Positive(t, sealed)
	assert.Positive(t, plain)
	assert.True(t, roomConverged(nodes, nil), "the room still agrees on everything")
}

// A sealed signal opens only for the member it went to, and only in its direction.
func TestSealedSignalOpensOnlyAtItsEnd(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 3)
	time.Sleep(500 * time.Millisecond)
	a, b, c := nodes[0], nodes[1], nodes[2]
	inner := []byte(`{"action":"ping"}`)

	a.cm.mu.Lock()
	sealed := a.cm.sealSignalLocked(a.cm.activeRoom, b.id, inner)
	a.cm.mu.Unlock()
	require.True(t, isSealedSignal(sealed))

	b.cm.mu.Lock()
	got, err := b.cm.openSignalLocked(b.cm.activeRoom, a.id, sealed)
	_, errBack := b.cm.openSignalLocked(b.cm.activeRoom, b.id, sealed)
	b.cm.mu.Unlock()
	require.NoError(t, err)
	assert.Equal(t, inner, got)
	assert.Error(t, errBack, "bound to who sent it")

	c.cm.mu.Lock()
	_, err = c.cm.openSignalLocked(c.cm.activeRoom, a.id, sealed)
	c.cm.mu.Unlock()
	assert.Error(t, err, "another member cannot open it")

	a.cm.mu.Lock()
	_, err = a.cm.openSignalLocked(a.cm.activeRoom, b.id, sealed)
	a.cm.mu.Unlock()
	assert.Error(t, err, "nor can it be reflected back to its sender")

	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	b.cm.mu.Lock()
	_, err = b.cm.openSignalLocked(b.cm.activeRoom, a.id, tampered)
	b.cm.mu.Unlock()
	assert.Error(t, err, "tampered")
}
