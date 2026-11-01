package main

import (
	"log/slog"
	"os"
)

// "Start with Windows": EchoFog starts when the user signs in, its window minimised. With
// "remember on this computer" (app_unlock.go) the profile then opens by itself and the node is
// back online with nobody at the keyboard, which is what people who share an exit or a relay from
// home need after a restart; without it the password screen waits. It is the user's choice, off
// by default, and per Windows user (app_autostart_windows.go). Other systems do not offer it yet.

// autostartFlag on the command line: the system started the app at sign-in, so the window opens
// minimised.
const autostartFlag = "--autostart"

// autostartName is the name of the entry the system starts the app from (tests use their own).
var autostartName = "EchoFog"

// AutostartDTO is what the app shows about starting with the system.
type AutostartDTO struct {
	Supported bool   `json:"supported"` // this system can start the app at sign-in (only Windows yet)
	Enabled   bool   `json:"enabled"`   // the app starts at sign-in
	Program   string `json:"program"`   // the program started then, when it is not this copy; "" otherwise
	Blocked   bool   `json:"blocked"`   // the entry is there, but switched off in the system's startup apps
}

// GetAutostart reports whether the app starts when the user signs in.
func (a *App) GetAutostart() AutostartDTO {
	exe, _ := os.Executable()
	return autostartState(autostartName, exe)
}

// SetAutostart switches starting at sign-in on, for this copy of the program, or off.
func (a *App) SetAutostart(on bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := setAutostart(autostartName, exe, on); err != nil {
		return err
	}
	slog.Info("Start at sign-in changed", "on", on, "program", exe)
	return nil
}

// takeAutostartFlag removes autostartFlag from the arguments after the program name and reports
// whether it was there.
func takeAutostartFlag(args []string) ([]string, bool) {
	found := false
	kept := make([]string, 0, len(args))
	for i, a := range args {
		if i > 0 && a == autostartFlag {
			found = true
			continue
		}
		kept = append(kept, a)
	}
	return kept, found
}
