// Package names finds EchoFog people by a name they own in a blockchain naming system (alice.ton,
// alice.t.me), instead of a 64-character key or a contact link. The name's owner writes a record
// into the name; the record carries the person's identity key and is signed by that key, so a name
// can only point at someone who agreed to it. See docs/design/crypto-usernames.md.
package names

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// A binding is the record a name holds, as text:
//
//	fogname1:<base64url of the body and the signature>
//
// body: version (1 byte) | issued, unix seconds (8, big endian) | identity public key (32) |
// owner length (1) | owner (n ≤ 64)
//
// The owner is the name's owner when the record was signed (TON: workchain byte and 32-byte
// account; Solana: the 32-byte key). A record left behind after the name changes hands names the
// old owner, so it reads as stale rather than as the new owner's. The Ed25519 signature by the
// identity key covers a fixed context, the name and the body.

// RecordPrefix starts a binding's text.
const RecordPrefix = "fogname1:"

const (
	bindingVersion = 1
	maxOwnerLen    = 64
	signContext    = "echofog name binding v1\x00"
)

var (
	// ErrNotBinding: the text is not an EchoFog name record at all.
	ErrNotBinding = errors.New("not an EchoFog name record")
	// ErrBadSignature: the record is not signed by the identity it names, or not for this name.
	ErrBadSignature = errors.New("the name record is not signed by the identity it names")
)

// Binding is what a name record says.
type Binding struct {
	Identity types.IdentityPublicKey
	Owner    []byte
	Issued   time.Time
	Sig      []byte
}

// NewBinding makes and signs the record that binds name to the identity pub. sign signs with the
// identity's private key (the key manager holds it, so it is not passed in).
func NewBinding(name string, owner []byte, pub types.IdentityPublicKey, issued time.Time, sign func([]byte) ([]byte, error)) (Binding, error) {
	name, _, err := Normalize(name)
	if err != nil {
		return Binding{}, err
	}
	if len(owner) == 0 || len(owner) > maxOwnerLen {
		return Binding{}, fmt.Errorf("owner of %d bytes", len(owner))
	}
	b := Binding{Identity: pub, Owner: append([]byte(nil), owner...), Issued: time.Unix(issued.Unix(), 0)}
	sig, err := sign(signedBytes(name, b.body()))
	if err != nil {
		return Binding{}, err
	}
	b.Sig = sig
	if err := b.Verify(name); err != nil {
		return Binding{}, err
	}
	return b, nil
}

func (b Binding) body() []byte {
	out := make([]byte, 0, 42+len(b.Owner))
	out = append(out, bindingVersion)
	out = binary.BigEndian.AppendUint64(out, uint64(b.Issued.Unix()))
	out = append(out, b.Identity[:]...)
	out = append(out, byte(len(b.Owner)))
	return append(out, b.Owner...)
}

func signedBytes(name string, body []byte) []byte {
	out := make([]byte, 0, len(signContext)+len(name)+1+len(body))
	out = append(out, signContext...)
	out = append(out, name...)
	out = append(out, 0)
	return append(out, body...)
}

// Text writes the record as it is stored in the name.
func (b Binding) Text() string {
	return RecordPrefix + base64.RawURLEncoding.EncodeToString(append(b.body(), b.Sig...))
}

// ParseBinding reads a record. It does not check the signature: Verify does, with the name.
func ParseBinding(text string) (Binding, error) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, RecordPrefix) {
		return Binding{}, ErrNotBinding
	}
	raw, err := base64.RawURLEncoding.DecodeString(text[len(RecordPrefix):])
	if err != nil {
		return Binding{}, fmt.Errorf("broken name record: %w", err)
	}
	if len(raw) < 42 {
		return Binding{}, errors.New("broken name record: too short")
	}
	if raw[0] != bindingVersion {
		return Binding{}, fmt.Errorf("name record of version %d: update the app to read it", raw[0])
	}
	var b Binding
	b.Issued = time.Unix(int64(binary.BigEndian.Uint64(raw[1:9])), 0)
	copy(b.Identity[:], raw[9:41])
	n := int(raw[41])
	if n == 0 || n > maxOwnerLen || len(raw) != 42+n+ed25519.SignatureSize {
		return Binding{}, errors.New("broken name record: bad length")
	}
	b.Owner = append([]byte(nil), raw[42:42+n]...)
	b.Sig = append([]byte(nil), raw[42+n:]...)
	return b, nil
}

// Verify checks that the identity signed this record for name.
func (b Binding) Verify(name string) error {
	name, _, err := Normalize(name)
	if err != nil {
		return err
	}
	if len(b.Sig) != ed25519.SignatureSize || !ed25519.Verify(b.Identity[:], signedBytes(name, b.body()), b.Sig) {
		return ErrBadSignature
	}
	return nil
}

// PeerID is the node the identity runs as (the SHA-256 of its key, as in contact links).
func PeerID(identity types.IdentityPublicKey) types.PeerID { return sha256.Sum256(identity[:]) }
