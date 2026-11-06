package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/call/callnet"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/mux"
	v2node "github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

type callSigner struct {
	priv ed25519.PrivateKey
	pub  types.PeerPublicKey
}

func (s *callSigner) CreateSignature(d []byte) ([]byte, error) { return ed25519.Sign(s.priv, d), nil }
func (s *callSigner) GetMyPubKey() types.PeerPublicKey         { return s.pub }

type noLinks struct{}

func (noLinks) Link([32]byte) (route.Link, bool) { return nil, false }

type noSignals struct{}

func (noSignals) SendSignal([32]byte, bool, []byte) error { return nil }

// callHost stands for the runtime: a real call transport and no sessions.
type callHost struct {
	tr    *callnet.Transport
	mu    sync.Mutex
	admit func([32]byte) bool
}

func (h *callHost) CallTransport() *callnet.Transport { return h.tr }
func (h *callHost) SetCallAdmit(f func([32]byte) bool) {
	h.mu.Lock()
	h.admit = f
	h.mu.Unlock()
}
func (h *callHost) Session(context.Context, v2node.DeviceID, v2node.DialOpt) (v2node.Peer, *mux.Session, error) {
	return v2node.Peer{}, nil, nil
}

func newV2CallApi(t *testing.T) (*NodeApi, *app.Calls) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := &callSigner{priv: priv}
	copy(s.pub[:], pub)
	h := &callHost{tr: callnet.New(callnet.Config{Links: noLinks{}, Signals: noSignals{}, Route: func() route.Route { return route.Auto }})}
	calls, err := app.NewCalls(app.CallsConfig{Host: h, Signer: s, Self: types.PeerPubKeyToID(s.pub)})
	require.NoError(t, err)
	api := newFacadeNode(t, config.ProtocolV2)
	api.AttachV2Calls(calls)
	t.Cleanup(func() { _ = api.LeaveCall() })
	return api, calls
}

// fakeEncoder stands for the native screen encoder (Media Foundation) that the call's rate
// control and keyframe requests reach.
type fakeEncoder struct {
	mu       sync.Mutex
	bitrates []uint64
	keys     int
}

func (e *fakeEncoder) SetBitrate(bps uint64) {
	e.mu.Lock()
	e.bitrates = append(e.bitrates, bps)
	e.mu.Unlock()
}
func (e *fakeEncoder) RequestKeyframe() { e.mu.Lock(); e.keys++; e.mu.Unlock() }

func TestAttachV2CallsSwitchesOnlyTheCallDomain(t *testing.T) {
	api, _ := newV2CallApi(t)
	require.IsType(t, &v2CallBackend{}, api.backends.call)
	require.IsType(t, v2PendingBackend{}, api.backends.chat)
	require.IsType(t, v2PendingBackend{}, api.backends.net, "the network domain is attached by AttachV2")

	v1 := newFacadeNode(t, config.ProtocolV1)
	v1.AttachV2Calls(&app.Calls{})
	require.IsType(t, &v1CallBackend{}, v1.backends.call, "protocol 1 ignores the v2 call layer")
}

func TestV2JoinGivesTheOfferAndTheRoster(t *testing.T) {
	api, calls := newV2CallApi(t)
	api.SetCallBackdrop("waves")
	offer, err := api.JoinCall("invite-secret", "")
	require.NoError(t, err)
	require.Contains(t, offer, "v=0")

	r := api.GetCallRoster()
	require.Equal(t, calls.Self().String(), r.Self)
	require.Equal(t, calls.Self().String(), r.Coordinator)
	require.Equal(t, "waves", r.Backdrops[calls.Self().String()])

	require.NoError(t, api.LeaveCall())
	require.Empty(t, api.GetCallRoster().CallID)
}

func TestV2SubmitAnswerWithoutACallFails(t *testing.T) {
	api, _ := newV2CallApi(t)
	require.Error(t, api.SubmitCallAnswer("v=0"))
	require.NotErrorIs(t, api.SubmitCallAnswer("v=0"), ErrV2NotBuilt)
}

func TestV2QualitiesFollowTheUploadCapacity(t *testing.T) {
	api, _ := newV2CallApi(t)
	require.Error(t, api.SetUploadCapacity(0))
	require.NoError(t, api.SetUploadCapacity(1500))
	var found bool
	for _, q := range api.ListStreamQualities() {
		if q.ID == "2160p60" {
			found = true
			require.False(t, q.Fits, "4K60 does not fit 1.5 Mbit/s")
		}
	}
	require.True(t, found, "the 4K60 preset is offered")
}

func TestV2StartStreamRaisesTheCapacityToTheChosenPreset(t *testing.T) {
	api, _ := newV2CallApi(t)
	_, err := api.JoinCall("invite-secret", "")
	require.NoError(t, err)
	require.Error(t, api.StartStream("s1", "screen", "no-such-preset"))
	require.NoError(t, api.SetUploadCapacity(1500))
	require.NoError(t, api.StartStream("s1", "screen", "2160p60"))
	for _, q := range api.ListStreamQualities() {
		if q.ID == "2160p60" {
			require.True(t, q.Fits, "the user's choice of a preset raises the declared capacity")
		}
	}
	r := api.GetCallRoster()
	require.Contains(t, r.Streams, "s1")
	require.NoError(t, api.StopScreenShare("s1"))
}

func TestV2StreamCallsOutsideACallAreErrors(t *testing.T) {
	api, _ := newV2CallApi(t)
	require.Error(t, api.StartStream("s1", "screen", "1080p60"))
	require.Error(t, api.WatchStream("s1"))
	require.Error(t, api.UnwatchStream("s1"))
	_, err := api.CallStreamStats("s1")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrV2NotBuilt)
}

func TestV2CaptureEncoderAndRTPBudget(t *testing.T) {
	api, _ := newV2CallApi(t)
	enc := &fakeEncoder{}
	require.NoError(t, api.SetCaptureEncoder("screen", enc))
	require.NoError(t, api.SetCaptureEncoder("screen", nil), "nil gives the role back to the webview")
	require.NotPanics(t, func() { api.PublishCaptureRTP("screen", []byte{0x80, 0x60, 0, 1}) }, "a packet outside a call is dropped")
	require.Equal(t, media.VideoUnitBudget(len("s1")), api.CaptureRTPBudget("s1"))
}

func TestV2CallbacksReachTheManager(t *testing.T) {
	api, calls := newV2CallApi(t)
	api.SetupCallCallbacks(func(string, bool, float32) {}, func(string, string, string, int) {}, func(string) {}, func(uint64, string) {})
	require.NotNil(t, calls.Manager().OnActiveSpeaker)
	require.NotNil(t, calls.Manager().OnStreamAnnounced)
	require.NotNil(t, calls.Manager().OnStreamClosed)
	require.NotNil(t, calls.Manager().OnTopologyUpdated)
}

func TestV2GroupCallsAndRelayChoiceStayPending(t *testing.T) {
	api, _ := newV2CallApi(t)
	_, err := api.StartGroupCall("g")
	require.ErrorIs(t, err, ErrV2NotBuilt)
	_, err = api.StartRoomCall("g", "r")
	require.ErrorIs(t, err, ErrV2NotBuilt)
	require.ErrorIs(t, api.SwitchCallRelay("x"), ErrV2NotBuilt)
}
