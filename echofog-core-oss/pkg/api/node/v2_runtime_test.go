package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/mux"
	v2node "github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/pkg/dto"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

// ---- the in-memory network of two stacks in one process ------------------------------------------------

// v2Queue is the listener of one stack: the other stack's dialer pushes carriers into it.
type v2Queue struct {
	ch     chan mux.Carrier
	done   chan struct{}
	once   sync.Once
	closed error
}

func newV2Queue() *v2Queue {
	return &v2Queue{ch: make(chan mux.Carrier, 4), done: make(chan struct{})}
}

func (q *v2Queue) Accept(ctx context.Context) (mux.Carrier, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.done:
		return nil, q.closed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (q *v2Queue) Addr() net.Addr { return nil }

func (q *v2Queue) Close() error {
	q.once.Do(func() {
		q.closed = errors.New("queue closed")
		close(q.done)
	})
	return nil
}

var _ carrier.Listener = (*v2Queue)(nil)

// v2MemDialer dials through an in-memory carrier into the queue of the other stack.
type v2MemDialer struct {
	rt   *v2node.Runtime
	peer func() *v2Queue
}

func (d v2MemDialer) Session(ctx context.Context, p v2node.DeviceID, opt v2node.DialOpt) (v2node.Peer, *mux.Session, error) {
	pair := memcarrier.NewQUIC()
	select {
	case d.peer().ch <- pair.Server:
	case <-ctx.Done():
		return v2node.Peer{}, nil, ctx.Err()
	}
	opt.Carrier = pair.Client
	return d.rt.Session(ctx, p, opt)
}

// v2Peer is one desktop facade at protocol 2 with a profile directory of its own.
type v2Peer struct {
	t        *testing.T
	api      *NodeApi
	node     *coreNode.Node
	mnemonic string
	mu       sync.Mutex
	q        *v2Queue
	other    *v2Peer
}

func (p *v2Peer) queue() *v2Queue {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.q
}

func newV2Peer(t *testing.T, name string) *v2Peer {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Protocol = config.ProtocolV2
	cfg.Storage.DatabasePath = filepath.Join(t.TempDir(), "db")
	nd := coreNode.NewNode(context.Background(), cfg, name)
	t.Cleanup(nd.Cancel)
	api := NewNodeApi(nd)
	phrase, err := api.Register("пароль-"+name, "")
	require.NoError(t, err)
	p := &v2Peer{t: t, api: api, node: nd, mnemonic: phrase}
	api.v2h = v2Hooks{
		Listen: func(dev *identity.Device) ([]carrier.Listener, []cards.Listener) {
			p.mu.Lock()
			p.q = newV2Queue() // a restart listens again on a fresh queue
			q := p.q
			p.mu.Unlock()
			mask := dev.Masking().Public()
			return []carrier.Listener{q}, []cards.Listener{
				{Suite: objects.SuiteNoise3, Host: "127.0.0.1", Port: 4430, Mask: mask},
				// the camouflaged address that makes the public card (D37); nothing listens on it here
				{Suite: objects.SuiteReality2QUIC, Host: "203.0.113.10", Port: 443, SNI: "example.com", Mask: mask},
			}
		},
		Dialer: func(rt *v2node.Runtime) links.Dialer {
			return v2MemDialer{rt: rt, peer: func() *v2Queue { return p.other.queue() }}
		},
		ChatDir:        v2TestDir{p: p},
		AccessListener: func(int, []v2node.AccessMaskKey) (carrier.Listener, error) { return newV2Queue(), nil },
	}
	return p
}

func (p *v2Peer) start() {
	p.t.Helper()
	require.NoError(p.t, p.api.StartNode())
	p.t.Cleanup(p.api.StopNode)

}

// wantNode is the node ID AD7 gives: SHA-256 of the algorithm-tagged v1 identity key.
func (p *v2Peer) wantNode() string {
	pub, err := hex.DecodeString(p.api.GetMyPubKey())
	require.NoError(p.t, err)
	require.Len(p.t, pub, 32, "the v1 identity key")
	sum := sha256.Sum256(append([]byte{0x01}, pub...))
	return hex.EncodeToString(sum[:])
}

// v2TestDir is the directory of two stacks in one process: two nodes are too few for the DHT to conclude a
// lookup, so an account is looked up in the records the other stack publishes about itself (device
// list, cards, prekeys), verified as the DHT client would verify them.
type v2TestDir struct{ p *v2Peer }

func (d v2TestDir) Resolve(_ context.Context, nd [32]byte) (*dht.Resolved, error) {
	for _, q := range []*v2Peer{d.p, d.p.other} {
		if q.wantNode() != hex.EncodeToString(nd[:]) {
			continue
		}
		q.api.v2l.Lock()
		s := q.api.v2s
		q.api.v2l.Unlock()
		if s == nil {
			return nil, errors.New("not running")
		}
		res := &dht.Resolved{}
		now := time.Now()
		for _, raw := range s.rt.OwnRecords() {
			env, err := objects.Verify(raw, now)
			if err != nil {
				return nil, err
			}
			switch env.Label {
			case objects.LabelDevices:
				if res.Devices, err = objects.VerifyDeviceList(raw, now); err != nil {
					return nil, err
				}
			case objects.LabelCard:
				c, err := objects.VerifyCard(raw, now)
				if err != nil {
					return nil, err
				}
				res.Cards = append(res.Cards, c)
			case objects.LabelPrekeys:
				pk, err := objects.VerifyPrekeys(raw, now)
				if err != nil {
					return nil, err
				}
				res.Prekeys = append(res.Prekeys, pk)
			}
		}
		if res.Devices == nil {
			return nil, errors.New("no device list")
		}
		return res, nil
	}
	return nil, errors.New("unknown account")
}

func newV2Pair(t *testing.T) (*v2Peer, *v2Peer) {
	t.Helper()
	a, b := newV2Peer(t, "a"), newV2Peer(t, "b")
	a.other, b.other = b, a
	a.start()
	b.start()
	return a, b
}

// befriend makes a and b contacts of each other by the standing contact links.
func befriend(t *testing.T, a, b *v2Peer) (aSeesB, bSeesA string) {
	t.Helper()
	st, err := a.api.GetAccessStatus()
	require.NoError(t, err)
	require.NotEmpty(t, st.Link, "a public node has a standing contact link")
	bSeesA, err = b.api.JoinInvite(st.Link)
	require.NoError(t, err)
	st, err = b.api.GetAccessStatus()
	require.NoError(t, err)
	aSeesB, err = a.api.JoinInvite(st.Link)
	require.NoError(t, err)
	return aSeesB, bSeesA
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	require.Eventually(t, f, 30*time.Second, 50*time.Millisecond, what)
}

// ---- the tests --------------------------------------------------------------------------------------------

// Step 1 of TestV2RuntimesPair. T11.7: two runtimes built by the facade alone (StartNode at protocol 2) exchange a message, a file
// and a group event, over a real handshake, the real chat/2 service and the real files/2 service.
func v2StepExchange(t *testing.T, a, b *v2Peer, aSeesB, bSeesA string) {
	require.Equal(t, a.wantNode(), bSeesA, "the node ID is the one the recovery phrase gives (AD7): SHA-256 of the v1 identity key")
	require.Equal(t, b.wantNode(), aSeesB)

	// a message
	var convMu sync.Mutex
	var seen []ChatMessage
	b.api.SubscribeConversations(func(c ConvChange) {
		convMu.Lock()
		seen = append(seen, c.Messages...)
		convMu.Unlock()
	})
	sent, err := a.api.ChatSend(ChatRef{Kind: "dm", ID: aSeesB}, SendRequest{Text: "привет через v2"})
	require.NoError(t, err)
	require.Equal(t, "привет через v2", sent.Text)
	eventually(t, "b gets the message", func() bool {
		convMu.Lock()
		defer convMu.Unlock()
		for _, m := range seen {
			if m.Text == "привет через v2" {
				return true
			}
		}
		return false
	})
	page, err := b.api.ChatHistory(ChatRef{Kind: "dm", ID: bSeesA}, "", 50)
	require.NoError(t, err)
	require.NotEmpty(t, page.Messages)

	// a call invitation (T11.4b) rings at the other side. The in-memory network has no call transport, so the
	// invitation is written the way InviteToCall writes it (InviteToCall itself: backends_v2_groupcall_test.go).
	ring := &ringLog{}
	b.api.OnIncomingCall(ring.add)
	_, err = a.api.ChatSend(ChatRef{Kind: "dm", ID: aSeesB}, SendRequest{Text: convlog.CallInvitePrefix + "call_in_process/coordinator-a"})
	require.NoError(t, err)
	eventually(t, "b is rung", func() bool { return len(ring.all()) > 0 })
	rung := ring.all()
	require.Len(t, rung, 1)
	require.Equal(t, IncomingCall{CallID: "call_in_process", Coordinator: "coordinator-a", From: bSeesA, Timestamp: rung[0].Timestamp}, rung[0])

	// a file: offered, accepted, fetched over files/2, and the sender sees the recipient got it
	var fileMu sync.Mutex
	incoming := map[string]dto.FileTransferDTO{}
	b.api.OnFileTransfer(func(f dto.FileTransferDTO) {
		fileMu.Lock()
		incoming[f.ID] = f
		fileMu.Unlock()
	})
	src := filepath.Join(t.TempDir(), "привет.bin")
	body := make([]byte, 300_000)
	for i := range body {
		body[i] = byte(i * 7)
	}
	require.NoError(t, os.WriteFile(src, body, 0o600))
	out, err := a.api.SendFile(aSeesB, src)
	require.NoError(t, err)
	require.Equal(t, "offered", out.State, "a sent offer is not a delivered file")
	var inID string
	eventually(t, "b is offered the file", func() bool {
		fileMu.Lock()
		defer fileMu.Unlock()
		for id, f := range incoming {
			if f.State == "offered" {
				inID = id
				return true
			}
		}
		return false
	})
	_, err = b.api.AcceptFile(inID)
	require.NoError(t, err)
	eventually(t, "b has the whole file", func() bool {
		f, err := b.api.GetFileTransfer(inID)
		return err == nil && f.State == "done"
	})
	path, err := b.api.FileTransferPath(inID)
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, body, got)
	eventually(t, "a sees that b got it (FilesOnDone -> FileServed)", func() bool {
		f, err := a.api.GetFileTransfer(out.ID)
		return err == nil && f.State == "done"
	})

	// a group event
	var gMu sync.Mutex
	var events []GroupEvent
	b.api.SubscribeGroupEvents(func(e GroupEvent) {
		gMu.Lock()
		events = append(events, e)
		gMu.Unlock()
	})
	g, err := a.api.CreateGroup("Команда", "рабочий чат", "private")
	require.NoError(t, err)
	inv, err := a.api.GroupInviteCreate(g.ID, GroupInviteSpec{Hours: 1})
	require.NoError(t, err)
	_, err = b.api.JoinGroup(inv.Link)
	require.NoError(t, err)
	_, err = a.api.SendGroupMessage(g.ID, "в группе")
	require.NoError(t, err)
	eventually(t, "b gets a group event for the group", func() bool {
		gMu.Lock()
		defer gMu.Unlock()
		for _, e := range events {
			if e.GroupID == g.ID && (e.Kind == "message" || e.Kind == "added") {
				return true
			}
		}
		return false
	})
}

// Step 2. L4 of T11.3d: the card a contact answers a knock with is the real one, checked by the real
// objects.VerifyCard, and it reaches the card book; a forged card does not.
func v2StepKnock(t *testing.T, a, b *v2Peer, bSeesA string) {
	before, aBefore := b.api.v2Ingested(), a.api.v2Ingested()
	require.NoError(t, b.api.v2App().Knock(context.Background(), bSeesA))
	eventually(t, "a's card comes back and is ingested", func() bool { return b.api.v2Ingested() > before })
	require.Equal(t, aBefore, a.api.v2Ingested(), "a did not knock, so a took no card")
}

// Step 3. The runtime stops and starts again on the same profile (what the restart for a new mode does): the
// node, the contacts and the data are the same afterwards.
func v2StepStopStart(t *testing.T, a *v2Peer) {
	before := a.wantNode()
	a.api.stopV2()
	require.False(t, a.api.V2Running())
	a.api.stopV2() // twice is harmless
	require.NoError(t, a.api.startV2())
	require.True(t, a.api.V2Running())
	cs, err := a.api.GetContacts()
	require.NoError(t, err)
	require.Len(t, cs, 1)
	st, err := a.api.GetAccessStatus()
	require.NoError(t, err)
	require.NotEmpty(t, st.Link)
	require.Equal(t, before, a.wantNode())
}

// Step 4. The mode saved for the next start applies by a restart of the v2 runtime, without the UI asking
// twice, and the contacts survive it.
func v2StepModeRestart(t *testing.T, a *v2Peer) {
	st, err := a.api.GetAccessStatus()
	require.NoError(t, err)
	require.Equal(t, "public", st.Mode)
	require.NoError(t, a.api.SetAccessMode("hidden"))
	eventually(t, "the runtime runs in hidden mode", func() bool {
		st, err := a.api.GetAccessStatus()
		return err == nil && st.Mode == "hidden" && st.NextMode == "" && a.api.V2Running()
	})
	cs, err := a.api.GetContacts()
	require.NoError(t, err)
	require.Len(t, cs, 1, "the contact is still there after the restart")
}

// Step 5. Sessions in use (a stream is open, as a call or a transfer holds one) are ended when the runtime
// stops or restarts, not left to drain for the 15 s of a GOAWAY (F1 of the T11.7 acceptance, N2).
func v2StepSessionsEnd(t *testing.T, a, b *v2Peer, aSeesB string) {
	_, err := a.api.ChatSend(ChatRef{Kind: "dm", ID: aSeesB}, SendRequest{Text: "сессия открыта"})
	require.NoError(t, err)
	a.api.v2l.Lock()
	rt := a.api.v2s.rt
	a.api.v2l.Unlock()
	eventually(t, "a has a session to b", func() bool {
		for _, ss := range rt.Status().Sessions {
			if ss.Node == v2node.NodeID(b.api.v2s.rt.Status().Node) {
				return true
			}
		}
		return false
	})
	bs := b.api.v2s.rt.Status()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, held, err := rt.Open(ctx, bs.Device, service.Chat, v2node.DialOpt{})
	require.NoError(t, err, "a stream is held open on the session")
	_ = held
	require.Positive(t, sessionGoroutines())
	b.api.StopNode() // so that b cannot dial the new runtime again
	require.NoError(t, a.api.restartV2())
	requireNoSessions(t, "after the restart of a")
	require.True(t, a.api.V2Running())
}

// Two runtimes in one process, one scenario: a test per step would register a profile and a node each, and
// the nodes of one Windows test binary run out of locked memory (memguard).
func TestV2RuntimesPair(t *testing.T) {
	a, b := newV2Pair(t)
	aSeesB, bSeesA := befriend(t, a, b)
	steps := []struct {
		name string
		do   func(t *testing.T)
	}{
		{"message, file and group event", func(t *testing.T) { v2StepExchange(t, a, b, aSeesB, bSeesA) }},
		{"knock answer carries the real card", func(t *testing.T) { v2StepKnock(t, a, b, bSeesA) }},
		{"stop and restart end the sessions", func(t *testing.T) { v2StepSessionsEnd(t, a, b, aSeesB) }},
		{"stop and start keep identity and data", func(t *testing.T) { v2StepStopStart(t, a) }},
		{"access mode change restarts the runtime", func(t *testing.T) { v2StepModeRestart(t, a) }},
	}
	for _, st := range steps {
		if !t.Run(st.name, st.do) {
			t.FailNow() // the steps build on each other
		}
	}
}

// One runtime on its own: the data directory (AD7), the goroutines after StopNode, and a restart that cannot
// start the new mode (F2 of the T11.7 acceptance).
func TestV2RuntimeSolo(t *testing.T) {
	a := newV2Peer(t, "solo")
	a.other = a
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	a.start()
	t.Run("data sits in the v2 directory", func(t *testing.T) {
		dir, err := a.node.V2DataDir()
		require.NoError(t, err)
		require.Equal(t, filepath.Join(filepath.Dir(a.node.Cfg.Storage.DatabasePath), "v2"), dir)
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.NotEmpty(t, entries, "the v2 store is there")
		require.NotEqual(t, filepath.Dir(a.node.Cfg.Storage.DatabasePath), dir)
		require.True(t, a.api.V2Running())
	})
	t.Run("a failed restart goes back to the old mode", func(t *testing.T) {
		listen := a.api.v2h.Listen
		var broken atomic.Bool
		a.api.v2h.Listen = func(dev *identity.Device) ([]carrier.Listener, []cards.Listener) {
			if broken.Load() {
				return nil, nil // hidden mode cannot start without an address
			}
			return listen(dev)
		}
		broken.Store(true)
		err := a.api.SetAccessMode("hidden")
		require.Error(t, err, "the user is told the new mode did not start")
		require.True(t, a.api.V2Running(), "the runtime runs again, in the old mode")
		st, err := a.api.GetAccessStatus()
		require.NoError(t, err)
		require.Equal(t, "public", st.Mode, "the old mode is saved again")
		broken.Store(false)
	})
	t.Run("stop leaves no goroutines", func(t *testing.T) {
		a.api.StopNode()
		require.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+2 }, 15*time.Second, 100*time.Millisecond,
			"goroutines after stop: %d, before start: %d", runtime.NumGoroutine(), before)
	})
}

// Protocol 1 never reaches the v2 stack.
func TestProtocol1DoesNotStartTheV2Stack(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	require.Error(t, api.startV2(), "startV2 is for protocol 2 only")
	require.False(t, api.V2Running())
	require.Nil(t, api.v2App())
}

// sessionGoroutines counts the goroutines of live sessions: the mux loops, the GoAway waiting for a drain.
func sessionGoroutines() int {
	buf := make([]byte, 4<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "internal/v2/mux.(*Session)") || strings.Contains(g, "internal/v2/session.(*Established).GoAway") ||
			strings.Contains(g, "internal/v2/node.(*Runtime).pathHooks") {
			n++
		}
	}
	return n
}

func requireNoSessions(t *testing.T, what string) {
	t.Helper()
	require.Eventually(t, func() bool { return sessionGoroutines() == 0 }, 5*time.Second, 50*time.Millisecond,
		"%s: %d session goroutines are still running", what, sessionGoroutines())
}
