package node

import (
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/node"
)

// "Remember on this computer" (see pkg/node/remember.go): the profile opens without its password
// for this OS user. Only Windows has it yet.

var (
	ErrRememberUnsupported = node.ErrRememberUnsupported
	ErrNotRemembered       = node.ErrNotRemembered
	ErrRememberedForeign   = node.ErrRememberedForeign
	ErrRememberedStale     = node.ErrRememberedStale
	ErrRememberedDamaged   = node.ErrRememberedDamaged
	ErrWrongPassword       = node.ErrWrongPassword
)

// RememberSupported reports whether this system can remember the unlock.
func RememberSupported() bool { return node.RememberSupported() }

// IsRemembered reports whether the named profile has a remembered unlock.
func IsRemembered(profileName string) bool {
	return node.IsRemembered(config.DefaultConfig(profileName))
}

// ForgetUnlock makes the named profile ask its password again at the next start.
func ForgetUnlock(profileName string) error {
	return node.ForgetUnlock(config.DefaultConfig(profileName))
}

// RememberUnlock keeps the unlock of this logged-in profile for the current OS user; appPassword
// must be its password. askPayments is kept with it.
func (n *NodeApi) RememberUnlock(appPassword string, askPayments bool) error {
	return n.node.RememberUnlock([]byte(appPassword), askPayments)
}

// RememberedUnlock reports whether this profile opens without its password here, and whether it
// asks the password before payments.
func (n *NodeApi) RememberedUnlock() (remembered, askPayments bool) {
	return n.node.RememberedUnlock()
}

// LoginRemembered logs in without the password; see node.LoginRemembered for the errors after
// which the caller asks the password.
func (n *NodeApi) LoginRemembered() (askPayments bool, err error) {
	return n.node.LoginRemembered()
}
