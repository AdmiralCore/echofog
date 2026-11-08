package node

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/stretchr/testify/require"
)

// fakeExitVPN is a fakeVPN of an app that can host an exit (T10.9).
type fakeExitVPN struct {
	*fakeVPN
	mu        sync.Mutex
	supported bool
	on        bool
	log       []string
	status    app.ExitStatusView
	err       error
}

func (f *fakeExitVPN) note(s string) { f.mu.Lock(); f.log = append(f.log, s); f.mu.Unlock() }

func (f *fakeExitVPN) ExitSupported() bool { return f.supported }
func (f *fakeExitVPN) ExitEnable(_ context.Context, s app.ExitSettings) (app.ExitInfo, error) {
	f.note("enable")
	if f.err != nil {
		return app.ExitInfo{}, f.err
	}
	f.mu.Lock()
	f.on = true
	f.status.Enabled = true
	f.mu.Unlock()
	return app.ExitInfo{Link: "echofog://join?v=2&card=EXITLINK", Web: "https://join.example.org/x"}, nil
}
func (f *fakeExitVPN) ExitDisable() error {
	f.note("disable")
	f.mu.Lock()
	f.on = false
	f.status.Enabled = false
	f.mu.Unlock()
	return nil
}
func (f *fakeExitVPN) ExitStatus() app.ExitStatusView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}
func (f *fakeExitVPN) ExitApprove(id, label string) error {
	f.note("approve:" + id + ":" + label)
	return f.err
}
func (f *fakeExitVPN) ExitReject(id string) error { f.note("reject:" + id); return f.err }
func (f *fakeExitVPN) ExitRevoke(id string) error { f.note("revoke:" + id); return f.err }
func (f *fakeExitVPN) ExitAllowAny(_ context.Context, ttl time.Duration) (app.ExitInfo, error) {
	f.note("any:" + ttl.String())
	return app.ExitInfo{Link: "echofog://join?v=2&card=EXITLINK", Until: 1_900_000_000}, f.err
}
func (f *fakeExitVPN) ExitRevokeAny() error { f.note("revoke-any"); return f.err }
func (f *fakeExitVPN) ExitLink(context.Context) (app.ExitInfo, error) {
	return app.ExitInfo{Link: "echofog://join?v=2&card=EXITLINK"}, nil
}

func newExitApi(t *testing.T, supported bool) (*NodeApi, *fakeExitVPN) {
	t.Helper()
	api := newFacadeNode(t, config.ProtocolV2)
	f := &fakeExitVPN{fakeVPN: &fakeVPN{}, supported: supported}
	api.AttachV2(&fakeV2App{})
	api.AttachV2VPN(f)
	return api, f
}

func TestV2SharingStartsAndStopsTheExit(t *testing.T) {
	api, f := newExitApi(t, true)
	require.False(t, api.GetSharingStatus().Enabled)
	require.NoError(t, api.StartSharing(SharingConfig{}))
	st := api.GetSharingStatus()
	require.True(t, st.Enabled)
	require.NotNil(t, st.Access)
	require.NoError(t, api.StopSharing())
	require.False(t, api.GetSharingStatus().Enabled)
	require.Equal(t, []string{"enable", "disable"}, f.log)
}

// A price is not part of the first exit: a client must not be told it is paying for something free.
func TestV2SharingRefusesAPrice(t *testing.T) {
	api, f := newExitApi(t, true)
	err := api.StartSharing(SharingConfig{PricePerMB: 49})
	require.Error(t, err)
	require.False(t, strings.Contains(err.Error(), "app:"))
	require.Empty(t, f.log, "nothing was started")
}

func TestV2SharingStatusCarriesTheLinkAndWhoIsThere(t *testing.T) {
	api, f := newExitApi(t, true)
	require.NoError(t, api.StartSharing(SharingConfig{}))
	f.mu.Lock()
	f.status = app.ExitStatusView{Enabled: true, Accepted: 3, Refused: 2, Bytes: 4096, RefusedBy: map[string]uint64{"policy": 2},
		Clients:  []app.ExitClientView{{ID: "aa", Label: "Anna", Streams: 2}, {ID: "bb", ByLink: true, Streams: 1}},
		Pending:  []app.ExitPendingView{{ID: "cc", Since: 10, Last: 20, Tries: 4}},
		Approved: []app.ExitApprovedView{{ID: "aa", Label: "Anna", Since: 5}},
		AnyUntil: 1_900_000_000}
	f.mu.Unlock()
	st := api.GetSharingStatus()
	require.EqualValues(t, 3, st.Accepted)
	require.EqualValues(t, 2, st.Refused)
	require.EqualValues(t, 4096, st.Bytes)
	require.EqualValues(t, 2, st.RefusedBy["policy"])
	require.NotNil(t, st.Access)
	require.Equal(t, "echofog://join?v=2&card=EXITLINK", st.Access.Link)
	require.EqualValues(t, 1_900_000_000, st.Access.AnyUntil)
	require.Len(t, st.Access.Clients, 2)
	require.Equal(t, "Anna", st.Access.Clients[0].Label)
	require.True(t, st.Access.Clients[1].ByLink)
	require.Len(t, st.Access.Waiting, 1)
	require.Equal(t, "cc", st.Access.Waiting[0].ID)
	require.Len(t, st.Access.Confirmed, 1)
}

func TestV2SharingAccessCalls(t *testing.T) {
	api, f := newExitApi(t, true)
	require.NoError(t, api.SharingApprove("aa", "Anna"))
	require.NoError(t, api.SharingReject("bb"))
	require.NoError(t, api.SharingRevoke("cc"))
	require.NoError(t, api.SharingOpenFor(30))
	require.NoError(t, api.SharingCloseOpen())
	require.Equal(t, []string{"approve:aa:Anna", "reject:bb", "revoke:cc", "any:30m0s", "revoke-any"}, f.log)
	// the time of the open access is bounded: the owner cannot open it for ever by accident
	require.Error(t, api.SharingOpenFor(0))
	require.Error(t, api.SharingOpenFor(-5))
	require.Error(t, api.SharingOpenFor(60*24*8))
	require.Len(t, f.log, 5)
}

func TestV2SharingErrorsAreNeutralRussian(t *testing.T) {
	api, f := newExitApi(t, true)
	for _, err := range []error{app.ErrExitNoEntry, app.ErrExitBadEntry, app.ErrExitFull, app.ErrExitNotOn, app.ErrExitBadTTL, app.ErrExitOff} {
		f.err = err
		got := api.SharingApprove("aa", "x")
		require.Error(t, got)
		require.Equal(t, app.UserMessage(err), got.Error())
		require.False(t, strings.Contains(got.Error(), "app:"), got.Error())
		require.NotEqual(t, app.UserMessage(context.DeadlineExceeded), got.Error(), "%v has its own text", err)
	}
}

func TestV2WithoutAHostTheExitIsNotThere(t *testing.T) {
	api, f := newExitApi(t, false)
	require.ErrorIs(t, api.StartSharing(SharingConfig{}), ErrV2NotBuilt)
	require.False(t, api.GetSharingStatus().Enabled)
	require.Empty(t, f.log)
}
