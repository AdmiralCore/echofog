package node_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

// fakeProtector stands in for the OS: a record sealed for one user opens only for that user and
// with the same entropy.
type fakeProtector struct{ user string }

func (f fakeProtector) Protect(secret, entropy []byte) ([]byte, error) {
	return append([]byte(f.user+"|"+string(entropy)+"|"), secret...), nil
}

func (f fakeProtector) Unprotect(blob, entropy []byte) ([]byte, error) {
	prefix := []byte(f.user + "|" + string(entropy) + "|")
	if !bytes.HasPrefix(blob, prefix) {
		return nil, errors.New("not sealed for this user")
	}
	return append([]byte(nil), blob[len(prefix):]...), nil
}

func rememberProfile(t *testing.T) *config.AppConfig {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = t.TempDir() + "/db"
	cfg.Network.BootstrapNodes = nil
	return cfg
}

// registered makes a profile with the password "right-pass", remembers its unlock and returns its
// identity.
func registered(t *testing.T, cfg *config.AppConfig, askPayments bool) types.PeerID {
	t.Helper()
	reg := node.NewNode(context.Background(), cfg, "reg")
	_, err := reg.Register([]byte("right-pass"), []byte(""))
	require.NoError(t, err)
	require.ErrorIs(t, reg.RememberUnlock([]byte("typo"), askPayments), node.ErrWrongPassword)
	require.False(t, node.IsRemembered(cfg), "a wrong password remembers nothing")
	require.NoError(t, reg.RememberUnlock([]byte("right-pass"), askPayments))
	id, err := reg.IdentityPeerID()
	require.NoError(t, err)
	reg.Stop()
	return id
}

// The next start logs in without the password, as the same identity, and says whether payments
// ask the password.
func TestRememberedUnlockLogsInWithoutThePassword(t *testing.T) {
	defer node.SetRememberProtector(fakeProtector{user: "alice"})()
	cfg := rememberProfile(t)
	want := registered(t, cfg, true)
	require.True(t, node.IsRemembered(cfg))

	n := node.NewNode(context.Background(), cfg, "auto")
	ask, err := n.LoginRemembered()
	require.NoError(t, err)
	require.True(t, ask)
	got, err := n.IdentityPeerID()
	require.NoError(t, err)
	require.Equal(t, want, got)
	remembered, ask := n.RememberedUnlock()
	require.True(t, remembered)
	require.True(t, ask)

	// remembering again replaces the setting
	require.NoError(t, n.RememberUnlock([]byte("right-pass"), false))
	_, ask = n.RememberedUnlock()
	require.False(t, ask)
	n.Stop()

	require.NoError(t, node.ForgetUnlock(cfg))
	require.False(t, node.IsRemembered(cfg))
	require.NoError(t, node.ForgetUnlock(cfg), "forgetting twice is fine")
	_, err = node.NewNode(context.Background(), cfg, "after").LoginRemembered()
	require.ErrorIs(t, err, node.ErrNotRemembered)
}

// A record another OS user sealed (or one copied from another computer) is dropped, and the
// password still opens the profile.
func TestRememberedUnlockOfAnotherUserFallsBack(t *testing.T) {
	defer node.SetRememberProtector(fakeProtector{user: "alice"})()
	cfg := rememberProfile(t)
	registered(t, cfg, true)

	node.SetRememberProtector(fakeProtector{user: "bob"})
	_, err := node.NewNode(context.Background(), cfg, "bob").LoginRemembered()
	require.ErrorIs(t, err, node.ErrRememberedForeign)
	require.False(t, node.IsRemembered(cfg), "the record that does not open is dropped")

	n := node.NewNode(context.Background(), cfg, "password")
	require.NoError(t, n.Login([]byte("right-pass")))
	n.Stop()
}

// A record that opens to something else is dropped.
func TestRememberedUnlockDamagedFallsBack(t *testing.T) {
	defer node.SetRememberProtector(fakeProtector{user: "alice"})()
	cfg := rememberProfile(t)
	registered(t, cfg, false)

	sealed, err := os.ReadFile(node.RememberFile(cfg))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(node.RememberFile(cfg), sealed[:len(sealed)-1], 0o600))
	_, err = node.NewNode(context.Background(), cfg, "short").LoginRemembered()
	require.ErrorIs(t, err, node.ErrRememberedDamaged)
	require.False(t, node.IsRemembered(cfg))
}

// After the root is sealed again (a new password) the kept key no longer opens it: it is dropped,
// and the database is released for the password screen.
func TestRememberedUnlockAfterNewPasswordFallsBack(t *testing.T) {
	defer node.SetRememberProtector(fakeProtector{user: "alice"})()
	cfg := rememberProfile(t)
	registered(t, cfg, true)

	n := node.NewNode(context.Background(), cfg, "change")
	require.NoError(t, n.Login([]byte("right-pass")))
	require.NoError(t, n.ResealIdentity([]byte("new-pass")))
	n.Stop()

	_, err := node.NewNode(context.Background(), cfg, "stale").LoginRemembered()
	require.ErrorIs(t, err, node.ErrRememberedStale)
	require.False(t, node.IsRemembered(cfg))

	n = node.NewNode(context.Background(), cfg, "password")
	require.NoError(t, n.Login([]byte("new-pass")), "the failed attempt must not keep the database")
	n.Stop()
}

// A failure that is not the record's (the database held by a running copy of the app) keeps it.
func TestRememberedUnlockKeptWhenTheDatabaseIsBusy(t *testing.T) {
	defer node.SetRememberProtector(fakeProtector{user: "alice"})()
	cfg := rememberProfile(t)
	registered(t, cfg, true)

	running := node.NewNode(context.Background(), cfg, "running")
	_, err := running.LoginRemembered()
	require.NoError(t, err)
	defer running.Stop()

	_, err = node.NewNode(context.Background(), cfg, "second").LoginRemembered()
	require.Error(t, err)
	require.True(t, node.IsRemembered(cfg), "a busy database is no reason to drop the record")
}

// Where the system has no protector the option is not there.
func TestRememberUnsupported(t *testing.T) {
	defer node.SetRememberProtector(nil)()
	require.False(t, node.RememberSupported())
	cfg := rememberProfile(t)
	n := node.NewNode(context.Background(), cfg, "reg")
	_, err := n.Register([]byte("right-pass"), []byte(""))
	require.NoError(t, err)
	require.ErrorIs(t, n.RememberUnlock([]byte("right-pass"), true), node.ErrRememberUnsupported)
	n.Stop()
}
