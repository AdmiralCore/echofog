package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/groups/senderkeys"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

// mesh stands in for the network of a group test the way the messenger behaves: a message to a
// member who is offline waits in the sender's outbox and leaves when both are online again (the
// sender must be online to resend it). Messages go through a queue that pump drains, so nodes
// answering nodes do not recurse, and a test can hold messages back to make things race.
type mesh struct {
	mu     sync.Mutex
	nodes  map[types.PeerID]*meshNode
	queue  []delivery
	held   []delivery // outboxes: waiting for the recipient (or the sender) to be online
	kinds  map[string]int
	paused bool      // nothing is handed over until resume
	clock  time.Time // the time of every node's key logic; tick moves it
}

type delivery struct {
	from, to types.PeerID
	text     string
}

type meshNode struct {
	f      *groupsFacade
	id     types.PeerID
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
	pubHex string
	store  *memStore
	online bool
	legacy bool // behaves like a node of the version before sealed messages
	m      *mesh
	evMu   sync.Mutex
	events []GroupEvent
}

func newMesh() *mesh {
	return &mesh{nodes: map[types.PeerID]*meshNode{}, kinds: map[string]int{}, clock: time.Now()}
}

func (m *mesh) now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clock
}

// tick moves the clock on and runs what the nodes' timers would: epoch replacements that are due.
func (m *mesh) tick(d time.Duration) {
	m.mu.Lock()
	m.clock = m.clock.Add(d)
	nodes := make([]*meshNode, 0, len(m.nodes))
	for _, n := range m.nodes {
		if n.online {
			nodes = append(nodes, n)
		}
	}
	m.mu.Unlock()
	for _, n := range nodes {
		n.f.runDueRekeys()
	}
	m.settle()
}

func (m *mesh) add(t testing.TB) *meshNode {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	n := &meshNode{id: types.PeerID(sha256.Sum256(pub)), pub: pub, priv: priv, pubHex: hex.EncodeToString(pub),
		store: newMemStore(), online: true, m: m}
	n.start()
	m.mu.Lock()
	m.nodes[n.id] = n
	m.mu.Unlock()
	return n
}

// start builds the node's group service on its store: a new node, or the same one restarted.
func (n *meshNode) start() {
	var ident types.IdentityPublicKey
	copy(ident[:], n.pub)
	id := n.id
	n.f = newGroupsFacade(n.store, id, ident, func(d []byte) ([]byte, error) { return ed25519.Sign(n.priv, d), nil },
		func(target types.PeerID, text string) (bool, error) { return n.m.send(id, target, text) },
		func(types.PeerID) bool { return true },
		func(ev GroupEvent) { n.evMu.Lock(); n.events = append(n.events, ev); n.evMu.Unlock() }, nil)
	n.f.now = n.m.now
}

var v2Kinds = map[string]bool{"smsg": true, "envs": true, "key": true, "keyreq": true, "sync": true}

func wireOf(text string) (groupWire, bool) {
	raw, ok := strings.CutPrefix(text, wirePrefix)
	if !ok {
		return groupWire{}, false
	}
	var w groupWire
	return w, json.Unmarshal([]byte(raw), &w) == nil
}

func (m *mesh) send(from, to types.PeerID, text string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := wireOf(text); ok {
		m.kinds[w.Kind]++
	}
	dst, src := m.nodes[to], m.nodes[from]
	if dst == nil || src == nil {
		return false, errUnknownPeer
	}
	d := delivery{from, to, text}
	if !dst.online || !src.online {
		m.held = append(m.held, d)
		return true, nil
	}
	m.queue = append(m.queue, d)
	return false, nil
}

type meshErr string

func (e meshErr) Error() string { return string(e) }

const errUnknownPeer = meshErr("unknown peer")

// pump hands over queued messages until none are left (and whatever they cause).
func (m *mesh) pump() {
	for {
		m.mu.Lock()
		if m.paused || len(m.queue) == 0 {
			m.mu.Unlock()
			return
		}
		d := m.queue[0]
		m.queue = m.queue[1:]
		dst, src := m.nodes[d.to], m.nodes[d.from]
		if dst == nil || src == nil {
			m.mu.Unlock()
			continue
		}
		if !dst.online {
			m.held = append(m.held, d)
			m.mu.Unlock()
			continue
		}
		text := d.text
		if dst.legacy || src.legacy {
			var ok bool
			if text, ok = toLegacy(text, dst.legacy); !ok {
				m.mu.Unlock()
				continue
			}
		}
		m.mu.Unlock()
		dst.f.handle(d.from, text)
	}
}

// toLegacy is what an older node sees of a message (or sends): no version, and none of the kinds
// it does not know.
func toLegacy(text string, toOld bool) (string, bool) {
	w, ok := wireOf(text)
	if !ok {
		return text, true
	}
	if toOld && v2Kinds[w.Kind] {
		return "", false
	}
	w.V = 0
	return encodeWire(w), true
}

// release moves held messages whose sender and recipient are both online back into the queue.
func (m *mesh) release() {
	m.mu.Lock()
	keep := m.held[:0]
	for _, d := range m.held {
		if m.nodes[d.to] != nil && m.nodes[d.from] != nil && m.nodes[d.to].online && m.nodes[d.from].online {
			m.queue = append(m.queue, d)
		} else {
			keep = append(keep, d)
		}
	}
	m.held = keep
	m.mu.Unlock()
}

func (m *mesh) setOnline(n *meshNode, on bool) {
	m.mu.Lock()
	n.online = on
	m.mu.Unlock()
	if on {
		m.release()
		m.pump()
	}
}

func (m *mesh) settle() {
	m.release()
	m.pump()
}

func (m *mesh) kind(k string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kinds[k]
}

// syncAll has every online node ask for what it missed, repeating until nothing changes.
func (m *mesh) syncAll(rounds int) {
	for i := 0; i < rounds; i++ {
		m.mu.Lock()
		nodes := make([]*meshNode, 0, len(m.nodes))
		for _, n := range m.nodes {
			if n.online && !n.legacy {
				nodes = append(nodes, n)
			}
		}
		m.mu.Unlock()
		for _, n := range nodes {
			n.f.mu.Lock()
			n.f.paced = map[string]time.Time{} // a test round is not minutes apart
			n.f.mu.Unlock()
			n.f.SyncGroups()
		}
		m.settle()
	}
}

func (n *meshNode) texts(t testing.TB, group string) []string {
	t.Helper()
	list, err := n.f.Messages(group, 0)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, x.Text)
	}
	return out
}

func (n *meshNode) keyring(group string) *senderkeys.Keyring {
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	return n.f.keyring(group)
}

func (n *meshNode) current(group string) senderkeys.EpochID {
	if e := n.keyring(group).Current(); e != nil {
		return e.ID()
	}
	return senderkeys.EpochID{}
}

// joinAll has every node join the group with an invite made by the owner.
func joinAll(t testing.TB, m *mesh, owner *meshNode, group string, nodes ...*meshNode) {
	t.Helper()
	for _, n := range nodes {
		invite, err := owner.f.Invite(group)
		require.NoError(t, err)
		_, err = n.f.Join(invite)
		require.NoError(t, err)
		m.settle()
	}
}
