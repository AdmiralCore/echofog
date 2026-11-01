package node

import (
	"errors"
	"fmt"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/protection/core/identity"
)

// ChangePassword seals the identity root again with newPassword, after checking oldPassword. The
// root is the only thing sealed with the password (everything else derives from it), so this is
// the whole change: the identity, the recovery phrase, the wallet and the data stay as they are.
// The root gets a new salt, which the unlock remembered on this computer would no longer match:
// it is sealed again with the new password, and if that fails it is deleted, so the next start
// asks the password instead of failing.
func (n *Node) ChangePassword(oldPassword, newPassword []byte) error {
	if n.identity == nil || n.Storage == nil {
		return errors.New("node is not logged in")
	}
	if len(newPassword) == 0 {
		return errors.New("the new password is empty")
	}
	// UnlockKey, Save and RememberUnlock wipe what they are given: each gets its own copy
	key, err := n.identity.UnlockKey(append([]byte(nil), oldPassword...))
	if errors.Is(err, identity.ErrInvalidPass) {
		return ErrWrongPassword
	}
	if err != nil {
		return err
	}
	memguard.WipeBytes(key)

	// what the remembered unlock keeps, read while it still opens
	remembered, askPayments := n.RememberedUnlock()
	if err := n.identity.Save(append([]byte(nil), newPassword...)); err != nil {
		return fmt.Errorf("could not seal the identity with the new password: %w", err)
	}
	if remembered {
		if err := n.RememberUnlock(append([]byte(nil), newPassword...), askPayments); err != nil {
			n.Logger.Warn("The remembered unlock could not follow the new password; it is dropped and the next start asks the password", "err", err)
			_ = ForgetUnlock(n.Cfg)
		}
	}
	n.Logger.Info("App password changed")
	return nil
}
