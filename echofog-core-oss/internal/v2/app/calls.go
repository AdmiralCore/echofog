package app

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
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

// Calls owns the call manager of a v2 node (T9.4).
type Calls struct{ host CallHost }

// NewCalls builds the call manager over the host's call transport.
func NewCalls(cfg CallsConfig) (*Calls, error) { return &Calls{}, nil }

// Manager is the v1 call manager running over the v2 sessions.
func (c *Calls) Manager() *call.CallManager { return nil }

// Self is this node's ID in calls.
func (c *Calls) Self() types.PeerID { return types.PeerID{} }

// Join enters the room of an invite and returns the SDP offer of the local WebRTC bridge.
func (c *Calls) Join(callID, coordinator string, uploadKbps int, backdrop string) (string, error) {
	return "", errors.New("not implemented")
}

// Leave exits the room and stops admitting media.
func (c *Calls) Leave() error { return errors.New("not implemented") }

// NetDrops are the media datagrams the transport dropped sending and receiving.
func (c *Calls) NetDrops() (out, in uint64) { return 0, 0 }

func (c *Calls) connect(ctx context.Context, id types.PeerID) error { return nil }

type memberCache struct{}

func newMemberCache(src func() []types.PeerID, now func() time.Time, ttl time.Duration) *memberCache {
	return &memberCache{}
}

func (m *memberCache) allow(device [32]byte) bool { return true }
