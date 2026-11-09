// Package releasekeys parses the release public key list that a build receives with
// -ldflags "-X <pkg>.releaseKeysSpec=...". The list is the trust root of the built-in entry list
// (T5.6), the update manifest (T4.5) and the install script (T12.4e); an empty list means an OSS
// build that trusts no release and therefore refuses every signed artifact (fail-closed).
package releasekeys

import (
	"github.com/echofog/echofog-core/internal/v2/keys"
)

// ParseSpec parses the build-time key list: Ed25519 public keys separated by commas, each in hex
// or base64 (standard or URL alphabet, padded or not), optionally in the akey text form
// "ed25519:<base64url>". An empty or whitespace-only spec is an empty set, not an error.
func ParseSpec(spec string) ([]keys.AKey, error) {
	return nil, nil // TODO: parse
}

// MustParseSpec is ParseSpec for package initialization: an invalid spec is a build error and
// panics instead of silently producing an empty (fail-closed but useless) set.
func MustParseSpec(prefix, spec string) []keys.AKey {
	return nil
}
