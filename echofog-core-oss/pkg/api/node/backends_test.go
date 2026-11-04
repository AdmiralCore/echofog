package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/dto"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

func newFacadeNode(t *testing.T, protocol int) *NodeApi {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Protocol = protocol
	cfg.Storage.DatabasePath = filepath.Join(t.TempDir(), "db")
	node := coreNode.NewNode(context.Background(), cfg, "api-backends")
	t.Cleanup(node.Cancel)
	return NewNodeApi(node)
}

// Protocol 1 keeps the facade itself as every backend: no code moved, no behaviour changed.
func TestBackendsProtocol1IsTheFacade(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	require.Same(t, api, api.backends.net)
	require.Same(t, api, api.backends.contact)
	require.Same(t, api, api.backends.chat)
	require.Same(t, api, api.backends.group)
	require.Same(t, api, api.backends.file)
	require.Same(t, api, api.backends.call)
	require.Same(t, api, api.backends.vpn)
}

// Protocol 2 selects the not-built backends, whose network methods answer the agreed message.
func TestBackendsProtocol2AnswerNotBuilt(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	require.IsType(t, v2PendingBackend{}, api.backends.net)
	require.IsType(t, v2PendingBackend{}, api.backends.chat)
	require.IsType(t, v2PendingBackend{}, api.backends.vpn)

	err := api.backends.net.ConnectToPeer("127.0.0.1:1/deadbeef")
	require.ErrorIs(t, err, ErrV2NotBuilt)
	require.EqualError(t, err, "v2 ещё не собран")

	require.Nil(t, api.backends.net.GetPeers(), "a method without an error result returns the zero value")
}

// Until T11.1b-e move the bodies, the facade methods themselves answer the same way: a network
// method with an error result says why it cannot run, one without returns the zero value, and the
// local settings keep working.
func TestProtocol2FacadeAnswersNotBuilt(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	require.ErrorIs(t, api.v2Unavailable(), ErrV2NotBuilt)
	require.Equal(t, config.ProtocolV2, api.GetProtocol())

	_, err := api.GetContacts()
	require.ErrorIs(t, err, ErrV2NotBuilt)
	err = api.ConnectToPeer("127.0.0.1:1/deadbeef")
	require.ErrorIs(t, err, ErrV2NotBuilt)
	require.Nil(t, api.GetPeers(), "no error result: the zero value")

	_, err = api.GetSettings()
	require.NoError(t, err, "settings stay local and work")
	require.Equal(t, coreNode.DefaultPrivacy, api.GetPrivacy(), "privacy settings stay local")
}

func TestProtocol1FacadeIsAvailable(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	require.NoError(t, api.v2Unavailable())
}

// An unknown value in the config means v1: a half mode is worse than none.
func TestChooseBackendsTreatsUnknownAsV1(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	api.node.Cfg.Protocol = 9
	require.Same(t, api, chooseBackends(api.node.Cfg.ProtocolOrDefault(), api).net)
}

// Settings persist the mode for the next start; GetConfiguredConfig applies it before NewNode, so
// the desktop passes the mode into the node it builds.
func TestGetConfiguredConfigAppliesPersistedProtocol(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	profile := "p2"
	dir := filepath.Join(home, ".echofog", "profiles", profile)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ui_settings.json"), []byte(`{"protocol":2}`), 0o600))

	cfg := GetConfiguredConfig(profile)
	require.Equal(t, config.ProtocolV2, cfg.Protocol)
	require.True(t, cfg.IsV2())
}

// Saving ordinary settings must not drop the mode chosen for the next start.
func TestSaveSettingsPreservesProtocol(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig("")
	cfg.Storage.DatabasePath = filepath.Join(dir, "db")
	path := filepath.Join(dir, settingsFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"protocol":2}`), 0o600))

	settingsMu.Lock()
	err := saveSettingsLocked(cfg, func(p *persistedSettings) { p.SocketBufferMB = 7 })
	settingsMu.Unlock()
	require.NoError(t, err)

	var p persistedSettings
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &p))
	require.Equal(t, config.ProtocolV2, p.Protocol)
	require.Equal(t, 7, p.SettingsDTO.SocketBufferMB)
}

func TestGetConfiguredConfigDefaultsToV1(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := filepath.Join(home, ".echofog", "profiles", "p1")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ui_settings.json"), []byte(`{"protocol":9}`), 0o600))

	cfg := GetConfiguredConfig("p1")
	require.Equal(t, config.ProtocolV1, cfg.Protocol)
}

// stubNetBackend proves the facade methods of the moved domains call the chosen backend. It
// implements only the methods the test uses; the embedded interface satisfies the rest.
type stubNetBackend struct{ netBackend }

func (stubNetBackend) GetPeers() []dto.PeerDTO       { return []dto.PeerDTO{{PeerID: "stub"}} }
func (stubNetBackend) AddBootstrapNode(string) error { return errStubBackend }

// stubContactBackend is the same for the contacts domain.
type stubContactBackend struct{ contactBackend }

func (stubContactBackend) GetContacts() ([]dto.ContactDTO, error) {
	return []dto.ContactDTO{{PeerID: "stub"}}, nil
}
func (stubContactBackend) NewContact(dto.ContactDTO) error { return errStubBackend }

var errStubBackend = errors.New("stub backend")

// The facade methods of the moved domains are delegates. At protocol 2 they must reach the
// selected backend rather than answering "v2 not built" themselves, or a future v2 backend would
// be closed off (the inheritance of T11.1a, item 1).
func TestMovedDomainsDelegateAtProtocol2(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)

	api.backends.net = stubNetBackend{}
	require.Equal(t, []dto.PeerDTO{{PeerID: "stub"}}, api.GetPeers())
	require.ErrorIs(t, api.AddBootstrapNode("1.2.3.4:4001/"+peerA), errStubBackend)

	api.backends.contact = stubContactBackend{}
	got, err := api.GetContacts()
	require.NoError(t, err)
	require.Equal(t, []dto.ContactDTO{{PeerID: "stub"}}, got)
	require.ErrorIs(t, api.NewContact(dto.ContactDTO{}), errStubBackend)
}

// requireClosed fails when ch is nil or open: a receive must complete with ok == false at once.
func requireClosed[T any](t *testing.T, name string, ch <-chan T) {
	t.Helper()
	select {
	case _, ok := <-ch:
		require.False(t, ok, "%s must be closed", name)
	default:
		t.Fatalf("%s must be closed at protocol 2, not nil or open", name)
	}
}

// The desktop ranges over MessageHandler and PeerDiscoveryHandler. At protocol 2 there is
// nothing to deliver, so both must be closed rather than nil: a nil channel would hang those
// goroutines for the whole run (the inheritance of T11.1a, item 2).
func TestProtocol2HandlersReturnClosedChannels(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)

	requireClosed(t, "PeerDiscoveryHandler", api.PeerDiscoveryHandler())
	requireClosed(t, "MessageHandler", api.MessageHandler())
	requireClosed(t, "the pending net backend", api.backends.net.PeerDiscoveryHandler())
	requireClosed(t, "the pending chat backend", api.backends.chat.MessageHandler())
}
