package app

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/internal/v2/call/callnet"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/pkg/api/types"
)

var (
	// ErrCallConfig reports a CallsConfig without a host, a transport or a signer.
	ErrCallConfig = errors.New("app: call layer needs a host with a transport, and a signer")
	// ErrNoCall reports a call operation outside a call.
	ErrNoCall = errors.New("app: not in a call")
)

// defaultRelayCapacity is how many members a node of unknown reachability feeds in a call tree.
const defaultRelayCapacity = 2

// admitTTL bounds how stale the room roster is when media is admitted: a new member's media is
// dropped for at most this long, and one who left is dropped within it. It also bounds the roster
// reads to a few per second, however many datagrams arrive.
const admitTTL = 100 * time.Millisecond

// CallHost is the part of *node.Runtime the call layer of the app uses.
type CallHost interface {
	CallTransport() *callnet.Transport
	SetCallAdmit(f func(device [32]byte) bool)
	Session(ctx context.Context, peer node.DeviceID, opt node.DialOpt) (node.Peer, *mux.Session, error)
}

// CallsConfig builds a Calls.
type CallsConfig struct {
	Host   CallHost
	Signer signaling.Signer
	Self   types.PeerID
	// RelayCapacity is how many members this node can feed in a call tree (0: a leaf); nil means 2.
	RelayCapacity func() int
	// RelayOnly marks a node reachable only through relays.
	RelayOnly func() bool
	Now       func() time.Time
}

// Calls owns the call manager of a v2 node (T9.4): the v1 call layer (trees, layers, bridge to the
// webview) over the call transport of the v2 sessions. Media is admitted only from the members of
// the room this node is in.
type Calls struct {
	cfg  CallsConfig
	host CallHost
	cm   *call.CallManager
	now  func() time.Time

	mu sync.Mutex // serialises Join and Leave
}

// NewCalls builds the call manager over the host's call transport. The manager sends its signals
// plain: the session already seals them (call/2 carries them, T9.3b).
func NewCalls(cfg CallsConfig) (*Calls, error) {
	if cfg.Host == nil || cfg.Signer == nil || cfg.Host.CallTransport() == nil {
		return nil, ErrCallConfig
	}
	c := &Calls{cfg: cfg, host: cfg.Host, now: cfg.Now}
	if c.now == nil {
		c.now = time.Now
	}
	c.cm = call.NewCallManager(cfg.Host.CallTransport(), cfg.Signer, cfg.Self)
	c.cm.SetPlainSignals(true)
	c.cm.SetPeerConnector(c.connect)
	return c, nil
}

// Manager is the v1 call manager running over the v2 sessions.
func (c *Calls) Manager() *call.CallManager { return c.cm }

// Self is this node's ID in calls.
func (c *Calls) Self() types.PeerID { return c.cfg.Self }

// connect opens (or reuses) the session with a tree neighbour the call layer wants to reach.
func (c *Calls) connect(ctx context.Context, id types.PeerID) error {
	_, _, err := c.host.Session(ctx, node.DeviceID(id), node.DialOpt{})
	return err
}

// Join enters the room of an invite and returns the SDP offer of the local WebRTC bridge. The
// invite is the room secret; only its hash goes on the wire. An empty or malformed coordinator
// means this node coordinates, as in v1.
func (c *Calls) Join(callID, coordinator string, uploadKbps int, backdrop string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, in := c.cm.Snapshot(); in {
		return "", errors.New("app: already in a call")
	}
	var coord types.PeerID
	if b, err := hex.DecodeString(coordinator); err == nil && len(b) == len(coord) {
		copy(coord[:], b)
	}
	if coord == (types.PeerID{}) {
		coord = c.cfg.Self
	}
	roomID, roomKey := call.DeriveRoom(callID)
	profile := tree.PeerNodeProfile{
		PeerID:           c.cfg.Self,
		UploadKbps:       uploadKbps,
		MaxRelayChildren: c.relayCapacity(),
		RelayOnly:        c.cfg.RelayOnly != nil && c.cfg.RelayOnly(),
		Backdrop:         backdrop,
		RTT:              10 * time.Millisecond,
		LastHeartbeat:    c.now(),
	}
	cache := newMemberCache(c.roster, c.now, admitTTL)
	c.host.SetCallAdmit(cache.allow)
	if err := c.cm.JoinRoom(roomID, coord, roomKey, profile); err != nil {
		c.host.SetCallAdmit(nil)
		return "", err
	}
	offer, err := c.cm.GetBridgeOffer()
	if err != nil {
		_ = c.cm.LeaveRoom()
		c.host.SetCallAdmit(nil)
		return "", err
	}
	return offer, nil
}

func (c *Calls) relayCapacity() int {
	if c.cfg.RelayCapacity != nil {
		return c.cfg.RelayCapacity()
	}
	return defaultRelayCapacity
}

func (c *Calls) roster() []types.PeerID {
	s, ok := c.cm.Snapshot()
	if !ok {
		return nil
	}
	return s.Members
}

// Leave exits the room and stops admitting media.
func (c *Calls) Leave() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.cm.LeaveRoom()
	c.host.SetCallAdmit(nil)
	return err
}

// NetDrops are the media datagrams the transport dropped sending and receiving.
func (c *Calls) NetDrops() (out, in uint64) {
	s := c.host.CallTransport().Stats()
	return s.SendNoPath, s.DroppedRate
}

// memberCache answers "is this device in the room" from the room roster, read at most once per
// TTL: media arrives at thousands of datagrams a second and the roster is a copy under a lock.
type memberCache struct {
	src func() []types.PeerID
	now func() time.Time
	ttl time.Duration

	mu  sync.Mutex
	at  time.Time
	set map[[32]byte]struct{}
}

func newMemberCache(src func() []types.PeerID, now func() time.Time, ttl time.Duration) *memberCache {
	return &memberCache{src: src, now: now, ttl: ttl}
}

func (m *memberCache) allow(device [32]byte) bool {
	if m.src == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.now(); m.set == nil || t.Sub(m.at) >= m.ttl {
		next := make(map[[32]byte]struct{})
		for _, p := range m.src() {
			next[p] = struct{}{}
		}
		m.set, m.at = next, t
	}
	_, ok := m.set[device]
	return ok
}
