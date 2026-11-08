package node

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/stretchr/testify/require"
)

// fakeVPN stands for the app layer's VPN: it embeds the interface, so a call a test did not expect
// panics, and it records what the backend asked.
type fakeVPN struct {
	v2VPN

	mu      sync.Mutex
	calls   []string
	status  app.VPNStatus
	exits   []app.SavedExit
	history []app.VPNSession
	fail    error
}

func (f *fakeVPN) note(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fakeVPN) called(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == s {
			return true
		}
	}
	return false
}

func (f *fakeVPN) VPNStart(_ context.Context, link string) (app.VPNStatus, error) {
	f.note("start:" + link)
	return f.status, f.fail
}
func (f *fakeVPN) VPNStartSaved(_ context.Context, node string) (app.VPNStatus, error) {
	f.note("saved:" + node)
	return f.status, f.fail
}
func (f *fakeVPN) VPNStop() error               { f.note("stop"); return f.fail }
func (f *fakeVPN) VPNStatus() app.VPNStatus     { return f.status }
func (f *fakeVPN) VPNExits() []app.SavedExit    { return f.exits }
func (f *fakeVPN) VPNHistory() []app.VPNSession { return f.history }
func (f *fakeVPN) VPNClearHistory() error       { f.note("clear"); return f.fail }
func (f *fakeVPN) VPNForget(node string) error  { f.note("forget:" + node); return f.fail }
func (f *fakeVPN) VPNName(node, name string) error {
	f.note("name:" + node + ":" + name)
	return f.fail
}
func (f *fakeVPN) VPNFavorite(node string, on bool) error {
	f.note("fav:" + node + map[bool]string{true: ":on", false: ":off"}[on])
	return f.fail
}

func newVPNApi(t *testing.T) (*NodeApi, *fakeVPN) {
	t.Helper()
	api := newFacadeNode(t, config.ProtocolV2)
	f := &fakeVPN{}
	api.AttachV2(&fakeV2App{})
	api.AttachV2VPN(f)
	return api, f
}

// fakeV2App satisfies v2App for the network backend; only the calls a test makes are answered.
type fakeV2App struct{ v2App }

func (*fakeV2App) Subscribe(func(app.Notice)) func() { return func() {} }
func (*fakeV2App) LastUpdate() *app.UpdateView       { return nil }

func TestV2StartVPNViaStartsTheExitOfALink(t *testing.T) {
	api, f := newVPNApi(t)
	f.status = app.VPNStatus{Running: true}
	require.NoError(t, api.StartVPNVia("echofog://join?v=2&card=EXIT"))
	require.True(t, f.called("start:echofog://join?v=2&card=EXIT"))

	f.fail = app.ErrNotExit
	err := api.StartVPNVia("echofog://join?v=2&card=OTHER")
	require.Error(t, err)
	require.False(t, strings.Contains(err.Error(), "app:"), "an internal sentence is never shown")
	require.Equal(t, app.UserMessage(app.ErrNotExit), err.Error())
}

func TestV2GetVPNStatusMapsTheStateAndTheReasons(t *testing.T) {
	api, f := newVPNApi(t)
	require.Equal(t, VPNStatus{}, api.GetVPNStatus(), "a stopped VPN says nothing")

	f.status = app.VPNStatus{Running: true, Listen: "127.0.0.1:1080", Exit: strings.Repeat("e1", 32), Active: 3, Rx: 10, Tx: 20,
		ExitState: app.ExitOK, Refused: 2, RefusedBy: map[string]uint64{"policy": 1, "busy": 1}, LastReason: "busy", LastHost: "192.0.2.9:443", LastAt: 77}
	st := api.GetVPNStatus()
	require.True(t, st.Running)
	require.Equal(t, "127.0.0.1:1080", st.ListenAddr)
	require.Equal(t, strings.Repeat("e1", 32), st.RelayPeerID)
	require.EqualValues(t, 3, st.ActiveConns)
	require.EqualValues(t, 10, st.BytesRx)
	require.EqualValues(t, 20, st.BytesTx)
	require.True(t, st.TunnelReady, "the tunnel is ready once the exit is ok")
	require.Equal(t, "ok", st.ExitState)
	require.EqualValues(t, 2, st.Refused)
	require.Equal(t, "busy", st.LastRefusalReason)
	require.Equal(t, "192.0.2.9:443", st.LastRefusalHost)
	require.EqualValues(t, 77, st.LastRefusalAt)
	require.False(t, st.WholeComputerSupported)

	for _, state := range []string{"checking", "unreachable", "no-camouflage"} {
		f.status.ExitState = state
		got := api.GetVPNStatus()
		require.False(t, got.TunnelReady, state)
		require.Equal(t, state, got.ExitState)
	}
}

func TestV2VPNStartStopAndLast(t *testing.T) {
	api, f := newVPNApi(t)
	require.NoError(t, api.StartVPN(strings.Repeat("e1", 32)))
	require.True(t, f.called("saved:"+strings.Repeat("e1", 32)))
	require.NoError(t, api.StopVPN())
	require.NoError(t, api.StopVPNForSwitch())
	require.True(t, f.called("stop"))

	// ConnectLastExit takes the exit used last
	f.exits = []app.SavedExit{{Node: strings.Repeat("e2", 32), LastUsed: 9}, {Node: strings.Repeat("e3", 32), LastUsed: 5}}
	require.NoError(t, api.ConnectLastExit())
	require.True(t, f.called("saved:"+strings.Repeat("e2", 32)))
	f.exits = nil
	err := api.ConnectLastExit()
	require.Error(t, err)
	require.NotContains(t, err.Error(), "app:")
}

func TestV2SavedExitsAndHistoryUseTheDTOsOfV1(t *testing.T) {
	api, f := newVPNApi(t)
	f.exits = []app.SavedExit{{Node: strings.Repeat("e1", 32), Name: "Дом", Favorite: true, Active: true, Sessions: 2, LastUsed: 50, Rx: 1, Tx: 2,
		Refused: 3, RefusedBy: map[string]uint64{"policy": 3}}}
	saved := api.GetSavedExits()
	require.Equal(t, []SavedExit{{PeerID: strings.Repeat("e1", 32), Name: "Дом", Favorite: true, Active: true, Sessions: 2, LastUsed: 50, BytesRx: 1,
		BytesTx: 2, Refused: 3, RefusedBy: map[string]uint64{"policy": 3}, PriceKnown: true}}, saved, "every exit is free here")
	nodes := api.ListExitNodes()
	require.Len(t, nodes, 1)
	require.Equal(t, strings.Repeat("e1", 32), nodes[0].PeerID)
	require.True(t, nodes[0].Connected)
	require.False(t, nodes[0].PaymentRequired)
	require.EqualValues(t, 0, nodes[0].PricePerMB)

	require.NoError(t, api.SetExitName(strings.Repeat("e1", 32), "Дача"))
	require.NoError(t, api.SetExitFavorite(strings.Repeat("e1", 32), false))
	require.NoError(t, api.ForgetExit(strings.Repeat("e1", 32)))
	for _, c := range []string{"name:" + strings.Repeat("e1", 32) + ":Дача", "fav:" + strings.Repeat("e1", 32) + ":off", "forget:" + strings.Repeat("e1", 32)} {
		require.True(t, f.called(c), c)
	}

	f.history = []app.VPNSession{{Node: strings.Repeat("e1", 32), StartedAt: 10, EndedAt: 20, Rx: 5, Tx: 6, Refused: 1, RefusedBy: map[string]uint64{"no-path": 1}, EndReason: "stopped"}}
	h := api.GetVPNHistory()
	require.Len(t, h, 1)
	require.Equal(t, strings.Repeat("e1", 32), h[0].PeerID)
	require.EqualValues(t, 10, h[0].StartedAt)
	require.EqualValues(t, 20, h[0].EndedAt)
	require.Equal(t, map[string]uint64{"no-path": 1}, h[0].RefusedBy)
	require.Equal(t, "stopped", h[0].EndReason)
	require.True(t, h[0].PriceKnown)
	require.NoError(t, api.ClearVPNHistory())
	require.True(t, f.called("clear"))
}

func TestV2VPNErrorsAreUserTexts(t *testing.T) {
	api, f := newVPNApi(t)
	f.fail = errors.New("badger: disk is full at /home/x")
	err := api.StopVPN()
	require.Equal(t, "Не удалось выполнить действие", err.Error())
	f.fail = app.ErrNoExit
	require.Equal(t, app.UserMessage(app.ErrNoExit), api.ForgetExit("x").Error())
}

// Payment, the market and the whole-computer mode are not part of the free VPN.
func TestV2WhatTheFreeVPNDoesNotHaveIsNotBuilt(t *testing.T) {
	api, _ := newVPNApi(t)
	_, err := api.PayExit(context.Background(), "1")
	require.ErrorIs(t, err, ErrV2NotBuilt)
	require.ErrorIs(t, api.SetAutoPay(true, "1", "2"), ErrV2NotBuilt)
	require.ErrorIs(t, api.SetPayAsset("USDT"), ErrV2NotBuilt)
	require.False(t, api.WholeComputerSupported())
	require.False(t, api.KillSwitchOn())
}

func TestAttachV2VPNDoesNothingAtProtocol1(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	api.AttachV2VPN(&fakeVPN{})
	require.Nil(t, api.v2vpn())
}
