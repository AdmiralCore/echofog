//go:build !windows

package main

// registerLinkScheme: on macOS the scheme comes from the app bundle's Info.plist and on Linux from
// the .desktop file of the package, so the program has nothing to register.
func registerLinkScheme() {}
