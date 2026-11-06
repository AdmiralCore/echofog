package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/stretchr/testify/require"
)

// stubV2App implements the methods the tests use; the embedded interface satisfies the rest.
type stubV2App struct {
	v2App
	mode     app.Mode
	joinErr  error
	contacts []app.Contact
	invited  app.InviteRequest
	revoked  []string
}

func (s *stubV2App) SetMode(m app.Mode) error { s.mode = m; return nil }
func (s *stubV2App) AccessStatus(context.Context) (app.AccessStatus, error) {
	return app.AccessStatus{Mode: app.ModePublic, NextMode: app.ModeHidden, Link: "echofog://join?v=2&card=X", WebLink: "https://h/join/#join?v=2&card=X",
		Joined: []app.Joined{{Pass: "aabb", JoinedAt: time.Unix(5, 0), Connected: true}}}, nil
}
func (s *stubV2App) Preview(string) (app.LinkInfo, error) {
	return app.LinkInfo{Kind: "invitation", Node: "n1"}, nil
}
func (s *stubV2App) AddByLink(context.Context, string) (app.Contact, error) {
	if s.joinErr != nil {
		return app.Contact{}, s.joinErr
	}
	return app.Contact{Node: "n1"}, nil
}
func (s *stubV2App) Contacts() ([]app.Contact, error) { return s.contacts, nil }
func (s *stubV2App) Invite(_ context.Context, r app.InviteRequest) (app.Invitation, error) {
	s.invited = r
	return app.Invitation{Pass: "0102", Link: "echofog://join?v=2&card=Y&s=Z", Entries: r.Entries, Expires: time.Unix(100, 0)}, nil
}
func (s *stubV2App) Passes(context.Context) ([]app.PassView, error) {
	return []app.PassView{{ID: "0102", Kind: "invitation", State: "used", Created: time.Unix(1, 0), LinkExpires: time.Unix(9, 0), MaxEntries: 2}}, nil
}
func (s *stubV2App) RevokePass(_ context.Context, id string) (app.PassView, error) {
	s.revoked = append(s.revoked, id)
	return app.PassView{ID: id, State: "revoked"}, nil
}
func (s *stubV2App) NetStatus() app.NetStatus {
	return app.NetStatus{Running: true, NetClass: "censored", Sessions: 2, Relay: 1, Direct: 1,
		Peers: []app.PeerStatus{{Device: "aabbccdd", Path: "direct"}, {Device: "11223344", Path: "relay"}}}
}
func (s *stubV2App) Report() string { return "EchoFog connection report — stub" }
func (s *stubV2App) Subscribe(func(app.Notice)) func() { return func() {} }
func (s *stubV2App) LastUpdate() *app.UpdateView { return nil }

func TestAttachV2SwitchesTheNetworkAndContactsDomains(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	require.IsType(t, v2PendingBackend{}, api.backends.net)
	api.AttachV2(&stubV2App{})
	require.IsType(t, &v2NetBackend{}, api.backends.net)
	require.IsType(t, &v2ContactBackend{}, api.backends.contact)
	require.IsType(t, v2PendingBackend{}, api.backends.chat, "other domains stay pending")

	v1 := newFacadeNode(t, config.ProtocolV1)
	v1.AttachV2(&stubV2App{})
	require.IsType(t, &v1NetBackend{}, v1.backends.net, "protocol 1 ignores the v2 app")
}

func TestV2NetworkBackendDelegatesToTheApp(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	st := &stubV2App{}
	api.AttachV2(st)

	acc, err := api.GetAccessStatus()
	require.NoError(t, err)
	require.Equal(t, "public", acc.Mode)
	require.Equal(t, "hidden", acc.NextMode)
	require.Equal(t, "echofog://join?v=2&card=X", acc.Link)
	require.Equal(t, []JoinedPeer{{PeerID: "aabb", JoinedAt: 5, Connected: true}}, acc.Joined)

	require.NoError(t, api.SetAccessMode("hidden"))
	require.Equal(t, app.ModeHidden, st.mode)
	st.mode = ""
	err = api.SetAccessMode("isolated")
	require.Error(t, err)
	require.Empty(t, st.mode, "an unsupported mode must not reach the app")
	require.NotErrorIs(t, err, ErrV2NotBuilt)

	info, err := api.ParseInvite("x")
	require.NoError(t, err)
	require.Equal(t, InviteInfo{PeerID: "n1", Private: true, Protocol: 2}, info)
	id, err := api.JoinInvite("x")
	require.NoError(t, err)
	require.Equal(t, "n1", id)

	require.ErrorIs(t, api.ConnectToPeer("1.2.3.4:5/ab"), ErrV2NotBuilt, "an address connect has no v2 meaning yet")
}

func TestV2ErrorsReachTheUIAsRussianTextWithoutInternals(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	api.AttachV2(&stubV2App{joinErr: errors.New("dial 203.0.113.9:443: refused")})
	_, err := api.JoinInvite("x")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "203.0.113")
	require.Regexp(t, `[а-я]`, err.Error())
}

func TestV2PassesShowTheLinkOnlyAtCreation(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	st := &stubV2App{}
	api.AttachV2(st)
	p, err := api.CreatePass(PassRequest{Label: "Аня", Days: 7, Devices: 3})
	require.NoError(t, err)
	require.Equal(t, app.InviteRequest{Days: 7, Entries: 3}, st.invited)
	require.Equal(t, "echofog://join?v=2&card=Y&s=Z", p.Link)
	list, err := api.ListPasses()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Empty(t, list[0].Link, "a listed pass must not carry the invitation secret")
	require.Equal(t, "used_up", list[0].State)
	require.NoError(t, api.RemoveJoinedPeer("aabb"))
	require.Equal(t, []string{"aabb"}, st.revoked)
}

func TestV2ContactsAndBlocklist(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	api.AttachV2(&stubV2App{contacts: []app.Contact{{Node: "a", Alias: "Аня"}, {Node: "b", Alias: "Борис", Blocked: true, Added: time.Unix(7, 0)}}})
	got, err := api.GetContacts()
	require.NoError(t, err)
	require.Equal(t, []dto.ContactDTO{{PeerID: "a", PubKey: "a", Alias: "Аня"}}, got)
	bl, err := api.GetBlockedPeers()
	require.NoError(t, err)
	require.Len(t, bl, 1)
	require.Equal(t, "b", bl[0].PeerID)
	require.ErrorIs(t, api.NewContact(dto.ContactDTO{PeerID: "c"}), ErrV2NotBuilt, "contacts come by links")
}

func TestV2NetworkStatusHasNoAddresses(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	api.AttachV2(&stubV2App{})
	peers := api.GetPeers()
	require.Len(t, peers, 2)
	for _, p := range peers {
		require.Empty(t, p.Address)
		require.EqualValues(t, 2, p.ProtocolVersion)
	}
	require.Equal(t, NetworkStats{Online: true, ConnectedPeers: 2}, api.GetNetworkStats())
	g, err := api.GetGuardStatus()
	require.NoError(t, err)
	require.Equal(t, "censored", g.Mode)
	rep, err := api.ConnectionReport("0.3.0")
	require.NoError(t, err)
	require.Equal(t, "EchoFog connection report — stub", rep.Text)
	require.Equal(t, "0.3.0", rep.Version)
}
