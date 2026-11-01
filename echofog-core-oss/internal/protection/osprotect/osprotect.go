// Package osprotect keeps a small secret for the operating system's current user: only that user,
// on that computer, reads it back. It backs "remember on this computer", where the key that opens
// the identity is kept so the app starts without its password.
package osprotect

// Protector seals and opens a secret for the current OS user. The entropy is mixed in: a secret
// sealed with one entropy does not open with another.
type Protector interface {
	Protect(secret, entropy []byte) ([]byte, error)
	Unprotect(blob, entropy []byte) ([]byte, error)
}

// System is this platform's protector, nil where there is none yet.
func System() Protector { return system() }
