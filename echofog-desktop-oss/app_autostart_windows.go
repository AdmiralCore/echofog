//go:build windows

package main

import (
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Windows starts the programs named in the user's Run key when the user signs in; that needs no
// administrator rights and touches no other user.
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// startupApprovedPath is where Windows keeps the user's on/off for each Run entry ("Startup apps"
// in Settings and Task Manager): a binary value whose first byte is odd when it is off. No value
// means on.
const startupApprovedPath = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`

// autostartCommand is what the Run entry starts.
func autostartCommand(exe string) string { return `"` + exe + `" ` + autostartFlag }

// commandProgram is the program a Run entry's command line starts.
func commandProgram(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if rest, ok := strings.CutPrefix(cmd, `"`); ok {
		prog, _, _ := strings.Cut(rest, `"`)
		return prog
	}
	prog, _, _ := strings.Cut(cmd, " ")
	return prog
}

func autostartState(name, exe string) AutostartDTO {
	st := AutostartDTO{Supported: true}
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return st
	}
	cmd, _, err := k.GetStringValue(name)
	k.Close()
	if err != nil {
		return st
	}
	if startupSwitchedOff(name) {
		st.Blocked = true
		return st
	}
	st.Enabled = true
	if prog := commandProgram(cmd); exe == "" || !strings.EqualFold(filepath.Clean(prog), filepath.Clean(exe)) {
		st.Program = prog
	}
	return st
}

// startupSwitchedOff: the user switched the entry off in Windows' startup apps.
func startupSwitchedOff(name string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, startupApprovedPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetBinaryValue(name)
	return err == nil && len(v) > 0 && v[0]&1 == 1
}

// setAutostart writes the Run entry for exe, or removes it. Switching it on here also takes back
// an "off" set in Windows' startup apps: the user has just asked for it.
func setAutostart(name, exe string, on bool) error {
	if !on {
		if err := deleteValue(runKeyPath, name); err != nil {
			return err
		}
		return deleteValue(startupApprovedPath, name)
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	err = k.SetStringValue(name, autostartCommand(exe))
	k.Close()
	if err != nil {
		return err
	}
	return deleteValue(startupApprovedPath, name)
}

// deleteValue removes a value of the user's registry; one that is not there is not an error.
func deleteValue(path, name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}
