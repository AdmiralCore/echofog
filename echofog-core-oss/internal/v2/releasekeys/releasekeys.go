// Package releasekeys parses the release public key list that a build receives with
// -ldflags "-X <pkg>.releaseKeysSpec=...". The list is the trust root of the built-in entry list
// (T5.6), the update manifest (T4.5) and the install script (T12.4e); an empty list means an OSS
// build that trusts no release and therefore refuses every signed artifact (fail-closed).
package releasekeys

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/echofog/echofog-core/internal/v2/keys"
)

// ParseSpec parses the build-time key list: Ed25519 public keys separated by commas, each in hex
// or base64 (standard or URL alphabet, padded or not), optionally in the akey text form
// "ed25519:<base64url>". An empty or whitespace-only spec is an empty set, not an error.
func ParseSpec(spec string) ([]keys.AKey, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	parts := strings.Split(spec, ",")
	out := make([]keys.AKey, 0, len(parts))
	for i, p := range parts {
		k, err := ParseKey(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i+1, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// ParseKey parses one Ed25519 release public key: hex, standard or URL base64 (padded or not), or
// the akey text form. The key must be a canonical Ed25519 point of full order; a small-order key
// would make signature checks meaningless, so it is refused at build time.
func ParseKey(s string) (keys.AKey, error) {
	if s == "" {
		return nil, errors.New("empty key")
	}
	if strings.HasPrefix(s, "ed25519:") {
		k, err := keys.ParseText(s)
		if err != nil {
			return nil, err
		}
		return keys.ParseKind(k, keys.Ed25519)
	}
	raw, err := decode(s)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: %d bytes, need %d", keys.ErrMalformed, len(raw), ed25519.PublicKeySize)
	}
	k := keys.New(keys.Ed25519, raw)
	if err := keys.CheckEd25519Public(k.Key()); err != nil {
		return nil, err
	}
	return k, nil
}

// decode tries hex first when the text has the exact length of a hex key, then the four base64
// alphabets. The input is not echoed: a mistyped key must not end up in a build log.
func decode(s string) ([]byte, error) {
	if len(s) == hex.EncodedLen(ed25519.PublicKeySize) && isHex(s) {
		return hex.DecodeString(s)
	}
	var first error
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.Strict().DecodeString(s); err == nil {
			return b, nil
		} else if first == nil {
			first = err
		}
	}
	return nil, fmt.Errorf("%w: base64 or hex expected: %v", keys.ErrMalformed, first)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// MustParseSpec is ParseSpec for package initialization: an invalid spec is a build error and
// panics instead of silently producing an empty (fail-closed but useless) set.
func MustParseSpec(prefix, spec string) []keys.AKey {
	ks, err := ParseSpec(spec)
	if err != nil {
		panic(prefix + ": bad release keys spec: " + err.Error())
	}
	return ks
}
