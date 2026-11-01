//go:build !windows

package main

import "errors"

// Starting at sign-in is offered on Windows only yet: macOS would need a login item and Linux an
// XDG autostart file, so the app reports it as not supported and the Settings card is not shown.

func autostartState(string, string) AutostartDTO { return AutostartDTO{} }

func setAutostart(string, string, bool) error {
	return errors.New("starting with the system is not offered on this system")
}
