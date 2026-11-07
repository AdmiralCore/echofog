// Package convsync carries the conversation log of v1 (internal/services/convlog) over chat/2: the
// signed events and the version-vector sync travel as the content of end-to-end messages
// (PersonalMessageProto.conv_event and conv_sync, services.md 4.4), to the accounts of a direct chat or
// of a group, whatever device they use. The log decides everything about a conversation (order,
// membership, rights at the point of an event); this package only moves its bytes.
//
// Accounts are named by their Ed25519 public key (the log's author) and addressed by the node ID of
// that key (SHA-256 of its akey); the chat service picks the device.
package convsync

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/ratelimit"
	"github.com/echofog/echofog-core/internal/services/convlog"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/store"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Limits of the transport.
const (
	// MeshLimit is the group size up to which an author sends an event to every member.
	MeshLimit = 50
	// MaxFanout is how many members the author of an event in a larger group sends it to.
	MaxFanout = 8
	// PassOn is how many members a member of a large group passes a fresh event on to, once.
	PassOn = 3
	// EventBurst is how many events one peer may hand us in an instant; more are dropped and come with
	// the sync. EventRate is the refill per second.
	EventBurst = 200
	EventRate  = 20
)

// StubText is what the app shows for an event of a kind this build does not know.
const StubText = "Сообщение из новой версии приложения — обновите приложение"

// Net sends a message to an account; *chat.Service does (Send), tests use a queue.
type Net interface {
	Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) error
}

// Directory finds the public key of the account behind a node ID.
type Directory interface {
	Pub(ctx context.Context, node [32]byte) ([32]byte, error)
}

// Config configures an Engine.
type Config struct {
	// Pub is the account's Ed25519 public key; Sign signs with its private key.
	Pub  [32]byte
	Sign func([]byte) ([]byte, error)
	// Store is the namespace of the log; IDs blinds identifiers in its keys.
	Store store.KV
	IDs   *store.IDs
	// Rules tells the log about groups (founder, roles); nil means direct chats only.
	Rules convlog.GroupRules
	Net   Net
	Dir   Directory
	// Groups resolves a conversation id a peer names to a group we are in.
	Groups func(convlog.ConvID) (convlog.Conv, bool)
	// Members lists the public keys of a group's members.
	Members func(convlog.Conv) [][32]byte
	// SharedGroups lists the groups we and a peer are both in.
	SharedGroups func(peer [32]byte) []convlog.Conv
	Now          func() time.Time
}

// Engine moves the events of one account's conversation log.
type Engine struct {
	cfg   Config
	log   *convlog.Log
	self  types.PeerID
	now   func() time.Time
	limit *ratelimit.Keyed[[32]byte]

	mu     sync.Mutex
	byNode map[[32]byte][32]byte     // node ID -> public key, as far as learned
	byID   map[types.PeerID][32]byte // peer id -> public key
	passed map[string]bool           // events already passed on (large groups), bounded
}

// New opens the log.
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil || cfg.IDs == nil || cfg.Sign == nil || cfg.Net == nil || cfg.Dir == nil {
		return nil, errors.New("convsync: Store, IDs, Sign, Net and Dir are required")
	}
	e := &Engine{
		cfg: cfg, now: cfg.Now,
		byNode: map[[32]byte][32]byte{}, byID: map[types.PeerID][32]byte{}, passed: map[string]bool{},
		limit: ratelimit.NewKeyed[[32]byte](EventRate, EventBurst, 4096),
	}
	if e.now == nil {
		e.now = time.Now
	}
	e.log = convlog.New(&kvStorage{kv: cfg.Store, ids: cfg.IDs}, cfg.Pub[:], cfg.Sign)
	if cfg.Rules != nil {
		e.log.SetGroupRules(cfg.Rules)
	}
	e.self = e.log.Self()
	return e, nil
}

// Log is the conversation log.
func (e *Engine) Log() *convlog.Log { return e.log }

// NodeOf is the node ID of an account key: SHA-256 of its akey (a tag byte and the key).
func NodeOf(pub [32]byte) [32]byte { return keys.New(keys.Ed25519, pub[:]).ID() }

func peerIDOf(pub [32]byte) types.PeerID { return types.PeerPubKeyToID(types.PeerPublicKey(pub)) }

// learn remembers the public key of an account.
func (e *Engine) learn(pub [32]byte) {
	e.mu.Lock()
	e.byNode[NodeOf(pub)] = pub
	e.byID[peerIDOf(pub)] = pub
	e.mu.Unlock()
}

// PubOfPeer is the account key behind a peer id of a conversation, as far as this engine has met
// it.
func (e *Engine) PubOfPeer(peer types.PeerID) ([32]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	pub, ok := e.byID[peer]
	return pub, ok
}

// DM is the direct conversation with an account.
func (e *Engine) DM(peer [32]byte) convlog.Conv {
	e.learn(peer)
	return convlog.DM(e.self, peerIDOf(peer))
}

// recipients lists the accounts an event of c goes to, without ourselves.
func (e *Engine) recipients(c convlog.Conv) [][32]byte {
	var out [][32]byte
	if c.Kind == convlog.KindDM {
		e.mu.Lock()
		pub, ok := e.byID[c.Peer]
		e.mu.Unlock()
		if ok {
			out = append(out, pub)
		}
		return out
	}
	if e.cfg.Members == nil {
		return nil
	}
	for _, pub := range e.cfg.Members(c) {
		if pub != e.cfg.Pub {
			e.learn(pub)
			out = append(out, pub)
		}
	}
	return out
}

// pick chooses n of the list at random.
func pick(list [][32]byte, n int) [][32]byte {
	out := append([][32]byte(nil), list...)
	for i := len(out) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			break
		}
		out[i], out[int(j.Int64())] = out[int(j.Int64())], out[i]
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Publish sends an event this account appended to the others of the conversation: all of them, or at
// most MaxFanout of a large group (the rest get it from sync and from the ones that pass it on).
func (e *Engine) Publish(ctx context.Context, c convlog.Conv, raw []byte) error {
	to := e.recipients(c)
	if c.Kind == convlog.KindGroup && len(to) > MeshLimit {
		to = pick(to, MaxFanout)
	}
	return e.sendAll(ctx, to, &chatpb.PersonalMessageProto{ConvEvent: raw})
}

// sendAll sends one message to several accounts; it fails only when it reached none of them.
func (e *Engine) sendAll(ctx context.Context, to [][32]byte, pm *chatpb.PersonalMessageProto) error {
	var last error
	ok := len(to) == 0
	for _, pub := range to {
		if err := e.cfg.Net.Send(ctx, NodeOf(pub), pm); err != nil {
			last = err
			continue
		}
		ok = true
	}
	if !ok {
		return last
	}
	return nil
}

// pubOf finds the key of the account behind a node ID.
func (e *Engine) pubOf(ctx context.Context, node [32]byte) ([32]byte, bool) {
	e.mu.Lock()
	pub, ok := e.byNode[node]
	e.mu.Unlock()
	if ok {
		return pub, true
	}
	pub, err := e.cfg.Dir.Pub(ctx, node)
	if err != nil || NodeOf(pub) != node {
		return [32]byte{}, false
	}
	e.learn(pub)
	return pub, true
}

func (e *Engine) resolver() convlog.Resolver {
	if e.cfg.Groups == nil {
		return nil
	}
	return func(id convlog.ConvID) (convlog.Conv, bool) { return e.cfg.Groups(id) }
}

// Receive takes a message from the account behind node: a log event or a sync message. It reports
// whether the message was one of those.
func (e *Engine) Receive(ctx context.Context, from [32]byte, pm *chatpb.PersonalMessageProto) bool {
	switch {
	case len(pm.GetConvEvent()) > 0:
		e.onEvent(ctx, from, pm.GetConvEvent())
		return true
	case len(pm.GetConvSync()) > 0:
		e.onSync(ctx, from, pm.GetConvSync())
		return true
	}
	return false
}

func (e *Engine) onEvent(ctx context.Context, from [32]byte, raw []byte) {
	if !e.limit.AllowAt(from, 1, e.now()) {
		return // too many at once; the sync brings what was dropped
	}
	pub, ok := e.pubOf(ctx, from)
	if !ok {
		return
	}
	fromID := peerIDOf(pub)
	cid, ok := convlog.PeekConv(raw)
	if !ok {
		return
	}
	var c convlog.Conv
	switch {
	case convlog.DMConv(e.self, fromID) == cid:
		c = convlog.DM(e.self, fromID)
	case e.cfg.Groups != nil:
		if c, ok = e.cfg.Groups(cid); !ok || c.Kind != convlog.KindGroup {
			return
		}
	default:
		return
	}
	_, fresh, err := e.log.Ingest(c, raw)
	switch {
	case errors.Is(err, convlog.ErrNotParticipant) || errors.Is(err, convlog.ErrForbidden):
		// not taken as it came: what allows it may not have reached us yet; ask the sender
		e.askSync(ctx, from, c)
	case err == nil && fresh && c.Kind == convlog.KindGroup:
		e.passOn(ctx, c, from, raw)
	}
}

// passOn gives a fresh event of a large group to a few other members, once, so that it reaches everybody
// in a few hops instead of waiting for the sync.
func (e *Engine) passOn(ctx context.Context, c convlog.Conv, from [32]byte, raw []byte) {
	members := e.recipients(c)
	if len(members) <= MeshLimit {
		return // the author sent it to everybody
	}
	sum := sha256.Sum256(raw)
	key := string(sum[:])
	e.mu.Lock()
	if e.passed[key] {
		e.mu.Unlock()
		return
	}
	if len(e.passed) > 4096 {
		e.passed = map[string]bool{}
	}
	e.passed[key] = true
	e.mu.Unlock()
	var others [][32]byte
	for _, m := range members {
		if NodeOf(m) != from {
			others = append(others, m)
		}
	}
	_ = e.sendAll(ctx, pick(others, PassOn), &chatpb.PersonalMessageProto{ConvEvent: raw})
}

func (e *Engine) askSync(ctx context.Context, node [32]byte, c convlog.Conv) {
	b, err := convlog.EncodeSync(e.log.Want(c))
	if err != nil {
		return
	}
	_ = e.cfg.Net.Send(ctx, node, &chatpb.PersonalMessageProto{ConvSync: b})
}

func (e *Engine) onSync(ctx context.Context, from [32]byte, raw []byte) {
	pub, ok := e.pubOf(ctx, from)
	if !ok {
		return
	}
	m, err := convlog.DecodeSync(raw)
	if err != nil {
		return
	}
	// the log answers only a participant of the conversation asked about, and says nothing to anybody
	// else: a conversation we are not in and one the asker is not in look the same
	for _, reply := range e.log.HandleSync(peerIDOf(pub), m, e.resolver()) {
		b, err := convlog.EncodeSync(reply)
		if err != nil {
			continue
		}
		_ = e.cfg.Net.Send(ctx, from, &chatpb.PersonalMessageProto{ConvSync: b})
	}
}

// Hello opens a sync with a peer for the conversations both are in.
func (e *Engine) Hello(ctx context.Context, peer [32]byte) error {
	convs := []convlog.Conv{e.DM(peer)}
	if e.cfg.SharedGroups != nil {
		convs = append(convs, e.cfg.SharedGroups(peer)...)
	}
	b, err := convlog.EncodeSync(e.log.Hello(convs))
	if err != nil {
		return err
	}
	return e.cfg.Net.Send(ctx, NodeOf(peer), &chatpb.PersonalMessageProto{ConvSync: b})
}

// Stub is an event of a kind this build does not know: stored, synced and passed on, shown as a stub.
type Stub struct {
	ID      convlog.EventID
	Author  [32]byte
	Kind    uint32
	TsMs    int64
	Text    string
	lamport uint64
}

// Stubs lists the events of c whose kind this build does not know, in log order. The log keeps them
// (and counts them in its version vector and digest) but has no view of them.
func (e *Engine) Stubs(c convlog.Conv) []Stub {
	var out []Stub
	for _, raw := range e.log.Export(c, 64<<20) {
		ev, id, err := convlog.Decode(raw)
		if err != nil || convlog.KnownKind(ev.Kind) || !e.log.Accepted(c, id) {
			continue // known, or refused: nobody sees a stub of what the author had no right to write
		}
		var author [32]byte
		copy(author[:], ev.Author)
		out = append(out, Stub{ID: id, Author: author, Kind: ev.Kind, TsMs: ev.TsMs, Text: StubText, lamport: ev.Lamport})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].lamport != out[j].lamport {
			return out[i].lamport < out[j].lamport
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out
}
