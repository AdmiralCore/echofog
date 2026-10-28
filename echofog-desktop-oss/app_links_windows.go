//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

// registerLinkScheme makes echofog:// links open this program, for the current user only (no
// administrator rights): HKCU\Software\Classes\echofog. It rewrites the entry only when it points
// elsewhere, so the copy started last owns the links.
func registerLinkScheme() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	command := `"` + exe + `" "%1"`
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Classes\echofog\shell\open\command`, registry.QUERY_VALUE); err == nil {
		cur, _, _ := k.GetStringValue("")
		k.Close()
		if cur == command {
			return
		}
	}
	root, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\echofog`, registry.SET_VALUE)
	if err != nil {
		return
	}
	_ = root.SetStringValue("", "URL:EchoFog link")
	_ = root.SetStringValue("URL Protocol", "")
	root.Close()
	cmd, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\echofog\shell\open\command`, registry.SET_VALUE)
	if err != nil {
		return
	}
	_ = cmd.SetStringValue("", command)
	cmd.Close()
}
