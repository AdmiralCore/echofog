package app_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/call/callnet"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

type fakeSigner struct {
	priv ed25519.PrivateKey
	pub  types.PeerPublicKey
}

func newFakeSigner() *fakeSigner {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := &fakeSigner{priv: priv}
	copy(s.pub[:], pub)
	return s
}

func (s *fakeSigner) CreateSignature(d []byte) ([]byte, error) { return ed25519.Sign(s.priv, d), nil }
func (s *fakeSigner) GetMyPubKey() types.PeerPublicKey         { return s.pub }

type noLinks struct{}

func (noLinks) Link(device [32]byte) (route.Link, bool) { return nil, false }

type noSignals struct{}

func (noSignals) SendSignal(to [32]byte, lossy bool, payload []byte) error { return nil }

// fakeCallHost stands for *node.Runtime: a real call transport with no sessions behind it.
type fakeCallHost struct {
	mu       sync.Mutex
	tr       *callnet.Transport
	admit    func([32]byte) bool
	sessions []node.DeviceID
}

func newFakeCallHost() *fakeCallHost {
	h := &fakeCallHost{}
	h.tr = callnet.New(callnet.Config{Links: noLinks{}, Signals: noSignals{}, Route: func() route.Route { return route.Auto }})
	return h
}

func (h *fakeCallHost) CallTransport() *callnet.Transport { return h.tr }
func (h *fakeCallHost) SetCallAdmit(f func([32]byte) bool) {
	h.mu.Lock()
	h.admit = f
	h.mu.Unlock()
}
func (h *fakeCallHost) admitFn() func([32]byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.admit
}
func (h *fakeCallHost) Session(_ context.Context, p node.DeviceID, _ node.DialOpt) (node.Peer, *mux.Session, error) {
	h.mu.Lock()
	h.sessions = append(h.sessions, p)
	h.mu.Unlock()
	return node.Peer{}, nil, nil
}

func newCalls(t *testing.T) (*app.Calls, *fakeCallHost) {
	t.Helper()
	h := newFakeCallHost()
	s := newFakeSigner()
	c, err := app.NewCalls(app.CallsConfig{Host: h, Signer: s, Self: types.PeerPubKeyToID(s.pub)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Leave() })
	return c, h
}

func TestNewCallsRejectsAnIncompleteConfig(t *testing.T) {
	s := newFakeSigner()
	for name, cfg := range map[string]app.CallsConfig{
		"no host":   {Signer: s},
		"no signer": {Host: newFakeCallHost()},
	} {
		_, err := app.NewCalls(cfg)
		require.ErrorIs(t, err, app.ErrCallConfig, name)
	}
}

func TestCallsBuildsTheManagerOverTheHostTransport(t *testing.T) {
	c, _ := newCalls(t)
	require.NotNil(t, c.Manager())
	require.NotEqual(t, types.PeerID{}, c.Self())
}

func TestJoinGivesTheBridgeOfferAndAdmitsOnlyTheRoom(t *testing.T) {
	c, h := newCalls(t)
	require.Nil(t, h.admitFn(), "outside a call nothing is admitted")

	offer, err := c.Join("invite-secret", "", 8000, "waves")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(offer, "v=0"), "an SDP offer of the bridge")

	admit := h.admitFn()
	require.NotNil(t, admit, "the room is admitted from the moment it is joined")
	require.False(t, admit([32]byte{9}), "a stranger sends no media")
	snap, ok := c.Manager().Snapshot()
	require.True(t, ok)
	require.Equal(t, c.Self(), snap.Coordinator, "an empty coordinator means this node")

	require.NoError(t, c.Leave())
	require.Nil(t, h.admitFn(), "after leaving no media is admitted")
	_, ok = c.Manager().Snapshot()
	require.False(t, ok)
}

func TestJoinWithABadCoordinatorFallsBackToSelfLikeV1(t *testing.T) {
	c, _ := newCalls(t)
	_, err := c.Join("invite-secret", "not-hex", 0, "")
	require.NoError(t, err)
	snap, _ := c.Manager().Snapshot()
	require.Equal(t, c.Self(), snap.Coordinator)
}

func TestSecondJoinFailsAndKeepsTheRunningCallAdmitted(t *testing.T) {
	c, h := newCalls(t)
	_, err := c.Join("invite", "", 0, "")
	require.NoError(t, err)
	_, err = c.Join("invite", "", 0, "")
	require.Error(t, err, "already in a call")
	require.NotNil(t, h.admitFn(), "the call that is running keeps its admit")
}

func TestNetDropsReadTheTransportCounters(t *testing.T) {
	c, _ := newCalls(t)
	out, in := c.NetDrops()
	require.Zero(t, out)
	require.Zero(t, in)
}
