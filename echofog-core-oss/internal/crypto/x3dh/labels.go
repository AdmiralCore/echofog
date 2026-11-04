package x3dh

import "github.com/awnumar/memguard"

// Labels are the KDF info strings of a session. They are wire values: both sides must use the
// same ones, and a stored session keeps the ones it was made with.
//
// v1 uses V1Labels (T7.0); v2 passes the chat/2 labels of services §4.0 through Config, so the
// construction stays the same while the protocol version is named in the key schedule.
type Labels struct {
	// Session is the info of the X3DH session-key HKDF (v1: FOG_X3DH_V2).
	Session string
	// PQXDH replaces Session when the KEM part is present (v1:
	// FOG_PQXDH_X25519_SHA-256_ML-KEM-768_V1).
	PQXDH string
	// InitChains is the info of the initial-chain HKDF (v1: FOG_RATCHET_INIT_V2).
	InitChains string
	// Root is the info of the double-ratchet root KDF (v1: FOG_RATCHET_ROOT_V2).
	Root string
}

// V1Labels are the labels of v1 (v0.2.x) sessions. They must not change: sessions with v1 nodes
// are built with them.
var V1Labels = Labels{
	Session:    DOMAIN_SEPARATION,
	PQXDH:      infoPQXDH,
	InitChains: infoInitChains,
	Root:       infoRoot,
}

// resolved fills the labels left unset with the v1 ones, so a zero Config keeps v1 behaviour.
func (l Labels) resolved() Labels {
	out := V1Labels
	if l.Session != "" {
		out.Session = l.Session
	}
	if l.PQXDH != "" {
		out.PQXDH = l.PQXDH
	}
	if l.InitChains != "" {
		out.InitChains = l.InitChains
	}
	if l.Root != "" {
		out.Root = l.Root
	}
	return out
}

// IdentityKeySource gives the X3DH identity DH key. *keymanager.AccountManager satisfies it: v1
// derives the key at index 1' of the account. v2 passes its own source, which keeps a random key
// per device (objects §5).
type IdentityKeySource interface {
	GetEncryptionKey() (*memguard.Enclave, []byte, error)
}

// Config tunes one engine. The zero value is v1.
type Config struct {
	// Labels of the key schedule; an unset field means the v1 one.
	Labels Labels
	// Identity replaces the account derivation; nil means the account's key (v1).
	Identity IdentityKeySource
}
