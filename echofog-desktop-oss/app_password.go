package main

import (
	"errors"
	"fmt"
	"unicode/utf8"

	api "github.com/echofog/echofog-core/pkg/api/node"
)

// minPasswordLen is the shortest new app password, as on the create screen.
const minPasswordLen = 8

// ChangePassword replaces the app password of the logged-in profile. It takes the current one: a
// page in the window cannot change it on its own. The identity, recovery phrase, wallet and data
// stay as they are, and a profile remembered on this computer stays remembered under the new
// password.
func (a *App) ChangePassword(oldPassword, newPassword string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	if utf8.RuneCountInString(newPassword) < minPasswordLen {
		return fmt.Errorf("the new password needs at least %d characters", minPasswordLen)
	}
	if err := nodeAPI.ChangePassword(oldPassword, newPassword); err != nil {
		if errors.Is(err, api.ErrWrongPassword) {
			return errors.New("wrong app password")
		}
		return err
	}
	return nil
}
