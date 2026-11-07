// Package installkit is the release side of the install-script trust root (T12.4e): it signs the
// text release record in the "sshsig" format that the base-system tool "ssh-keygen -Y verify" checks,
// renders the record and the key-rotation record, and stamps the release public keys into install.sh.
// Nothing here is used by a running node.
package installkit

import (
	"errors"

	"github.com/echofog/echofog-core/internal/v2/keys"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Namespaces of the signatures: a signature of one kind never verifies as the other.
const (
	NSRelease  = "echofog-release-v1"
	NSRotation = "echofog-rotation-v1"
)

var errTODO = errors.New("installkit: not implemented")

// PublicLine is the OpenSSH public key line "ssh-ed25519 AAAA…" of a release key.
func PublicLine(k keys.AKey) (string, error) { return "", errTODO }

// SignSSH returns the armored sshsig signature of msg by s in the namespace.
func SignSSH(s *keys.Signer, namespace string, msg []byte) ([]byte, error) { return nil, errTODO }

// KeyBlock is the exact text between the BEGIN and END markers of install.sh: the sorted key
// lines and the key epoch. Its SHA-256 is the key fingerprint shown on the site and in the README.
func KeyBlock(ks []keys.AKey, epoch uint32) (string, error) { return "", errTODO }

// Fingerprint is the lower-case hex SHA-256 of KeyBlock.
func Fingerprint(ks []keys.AKey, epoch uint32) (string, error) { return "", errTODO }

// Stamp puts the key set, the epoch and the version floor into a template of install.sh.
func Stamp(script string, ks []keys.AKey, epoch uint32, floor string) (string, error) {
	return "", errTODO
}

// Record renders the text release record from a release manifest.
func Record(rel *pb.Release, epoch uint32) (string, error) { return "", errTODO }

// SignRecord signs data in the namespace by every signer and returns the concatenated armored blocks.
func SignRecord(data []byte, namespace string, signers ...*keys.Signer) ([]byte, error) {
	return nil, errTODO
}

// Rotation renders the record, to be signed by the old keys, that announces the new key set.
func Rotation(next []keys.AKey, nextEpoch uint32) (string, error) { return "", errTODO }
