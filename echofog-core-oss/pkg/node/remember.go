package node

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/protection/core/identity"
	"github.com/echofog/echofog-core/internal/protection/osprotect"
)

// "Remember on this computer": the key that opens the identity root is kept in the profile folder,
// sealed for the current OS user (Windows DPAPI), so the app starts without its password. What is
// kept is the key the password becomes through Argon2id, never the password: a password used
// elsewhere too does not leave this computer's disk. The password stays the way in on another
// computer, for another OS user, and once the option is off. Sealing the root again (a new
// password, a restore, the stronger password setting) gives it a new salt, and a key kept from
// before no longer opens it.

const rememberFileName = "remembered-unlock"

// rememberMagic starts the sealed record: format 1 is the magic, a flags byte, then the 32-byte key.
var rememberMagic = []byte("EFU1")

const (
	rememberKeyLen      = 32
	rememberAskPayments = 1 << 0 // the desktop asks the password before payments
)

// rememberProtector seals the remembered key; nil where the system has none yet. A variable so
// tests can stand in for it.
var rememberProtector = osprotect.System()

var (
	// ErrRememberUnsupported: this system cannot keep the unlock yet (only Windows can).
	ErrRememberUnsupported = errors.New("remembering the unlock is not supported on this system yet")
	// ErrNotRemembered: the profile has no remembered unlock.
	ErrNotRemembered = errors.New("no unlock is remembered for this profile")
	// ErrRememberedForeign: the remembered unlock does not open for this OS user: another account,
	// a profile folder copied from another computer, or a damaged file.
	ErrRememberedForeign = errors.New("the saved unlock does not open for this user on this computer")
	// ErrRememberedStale: it opens but no longer opens the identity, sealed again since.
	ErrRememberedStale = errors.New("the saved unlock no longer matches the password")
	// ErrRememberedDamaged: it opens to something that is not a remembered unlock.
	ErrRememberedDamaged = errors.New("the saved unlock is damaged")
)

// RememberSupported reports whether this system can remember the unlock.
func RememberSupported() bool { return rememberProtector != nil }

func rememberPath(cfg *config.AppConfig) string {
	return filepath.Join(filepath.Dir(cfg.Storage.DatabasePath), rememberFileName)
}

// rememberEntropy ties the sealed record to its profile: moved into another profile's folder it
// does not open.
func rememberEntropy(cfg *config.AppConfig) []byte {
	return []byte("echofog remembered unlock v1\x00" + filepath.Base(filepath.Dir(cfg.Storage.DatabasePath)))
}

// IsRemembered reports whether cfg's profile has a remembered unlock. The file is not opened: a
// bad one is found out, and dropped, by LoginRemembered.
func IsRemembered(cfg *config.AppConfig) bool {
	_, err := os.Stat(rememberPath(cfg))
	return err == nil
}

// ForgetUnlock deletes the remembered unlock of cfg's profile: the next start asks the password.
// Nothing remembered is not an error.
func ForgetUnlock(cfg *config.AppConfig) error {
	if err := os.Remove(rememberPath(cfg)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RememberUnlock keeps the key that opens this logged-in node's identity, sealed for the current
// OS user, so the next start logs in with LoginRemembered. appPassword must be this identity's;
// askPayments is kept along and given back by LoginRemembered. Called again, it replaces what was
// kept.
func (n *Node) RememberUnlock(appPassword []byte, askPayments bool) error {
	p := rememberProtector
	if p == nil {
		return ErrRememberUnsupported
	}
	if n.identity == nil || n.Storage == nil {
		return errors.New("node is not logged in")
	}
	key, err := n.identity.UnlockKey(appPassword)
	if errors.Is(err, identity.ErrInvalidPass) {
		return ErrWrongPassword
	}
	if err != nil {
		return err
	}
	defer memguard.WipeBytes(key)

	flags := byte(0)
	if askPayments {
		flags |= rememberAskPayments
	}
	record := append(append(append(make([]byte, 0, len(rememberMagic)+1+len(key)), rememberMagic...), flags), key...)
	defer memguard.WipeBytes(record)
	sealed, err := p.Protect(record, rememberEntropy(n.Cfg))
	if err != nil {
		return fmt.Errorf("could not seal the unlock for this user: %w", err)
	}

	path := rememberPath(n.Cfg)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// openRemembered reads and opens cfg's remembered unlock. A record that cannot serve is deleted,
// and the error says why.
func openRemembered(cfg *config.AppConfig) (key []byte, askPayments bool, err error) {
	p := rememberProtector
	if p == nil {
		return nil, false, ErrRememberUnsupported
	}
	sealed, err := os.ReadFile(rememberPath(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, ErrNotRemembered
	}
	if err != nil {
		return nil, false, err
	}
	record, err := p.Unprotect(sealed, rememberEntropy(cfg))
	if err != nil {
		_ = ForgetUnlock(cfg)
		return nil, false, ErrRememberedForeign
	}
	if len(record) != len(rememberMagic)+1+rememberKeyLen || !bytes.HasPrefix(record, rememberMagic) {
		memguard.WipeBytes(record)
		_ = ForgetUnlock(cfg)
		return nil, false, ErrRememberedDamaged
	}
	key = append([]byte(nil), record[len(rememberMagic)+1:]...)
	askPayments = record[len(rememberMagic)]&rememberAskPayments != 0
	memguard.WipeBytes(record)
	return key, askPayments, nil
}

// RememberedUnlock reports whether this node's profile has a remembered unlock that opens for this
// OS user, and whether it asks the password before payments. One that does not open is dropped.
func (n *Node) RememberedUnlock() (remembered, askPayments bool) {
	key, ask, err := openRemembered(n.Cfg)
	if err != nil {
		return false, false
	}
	memguard.WipeBytes(key)
	return true, ask
}

// LoginRemembered logs in with the remembered unlock instead of the password and gives back
// whether it asks the password before payments. A remembered unlock that cannot serve (another OS
// user or computer, the root sealed again, damage) is deleted and the error says which, so the
// caller falls back to the password with a reason; any other failure, such as a database another
// process holds, keeps it.
func (n *Node) LoginRemembered() (askPayments bool, err error) {
	if !n.IsRegistered() {
		return false, errors.New("node is not registered, please register first")
	}
	key, askPayments, err := openRemembered(n.Cfg)
	if err != nil {
		return false, err
	}
	defer memguard.WipeBytes(key)

	if err := n.initDB(); err != nil {
		return false, err
	}
	idMgr := identity.NewProtectedIdentity(n.Storage)
	if err := idMgr.LoadWithKey(key); err != nil {
		// release the database, as Login does: the password screen comes next and opens it again
		n.Storage.Close()
		n.Storage = nil
		if errors.Is(err, identity.ErrInvalidPass) {
			_ = ForgetUnlock(n.Cfg)
			return false, ErrRememberedStale
		}
		return false, fmt.Errorf("corrupted identity: %w", err)
	}

	n.identity = idMgr
	n.SetUserKey(append([]byte(nil), key...))
	n.IsReady = true
	n.Logger.Info("Login successful with the unlock remembered on this computer")
	return askPayments, nil
}
