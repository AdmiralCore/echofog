//go:build !windows

package osprotect

// The macOS Keychain and the Linux Secret Service are not wired up yet: until they are, these
// systems have no protector and "remember on this computer" is not offered there.
func system() Protector { return nil }
