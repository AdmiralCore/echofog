// Package installkit is the release side of the install-script trust root (T12.4e): it signs the
// text release record in the "sshsig" format that the base-system tool "ssh-keygen -Y verify" checks,
// renders the record and the key-rotation record, and stamps the release public keys into install.sh.
// Nothing here is used by a running node.
package installkit

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Namespaces of the signatures: a signature of one kind never verifies as the other.
const (
	NSRelease  = "echofog-release-v1"
	NSRotation = "echofog-rotation-v1"
)

const (
	beginMarker = "# BEGIN RELEASE KEYS\n"
	endMarker   = "# END RELEASE KEYS\n"
	// Threshold is the number of distinct release keys that must sign (D16).
	Threshold = 2
	maxKeys   = 5
	// maxArtifactSize bounds the size a record may promise (the script's curl limit is the same).
	maxArtifactSize = 256 << 20
)

var (
	platformRe = regexp.MustCompile(`^[a-z0-9]{2,12}-[a-z0-9]{2,12}$`)
	floorRe    = regexp.MustCompile(`(?m)^MIN_VERSION_FLOOR=.*$`)
)

func pubBlob(k keys.AKey) ([]byte, error) {
	if _, err := keys.ParseKind(k, keys.Ed25519); err != nil {
		return nil, fmt.Errorf("installkit: %w", err)
	}
	var b bytes.Buffer
	putString(&b, []byte("ssh-ed25519"))
	putString(&b, k.Key())
	return b.Bytes(), nil
}

func putString(b *bytes.Buffer, s []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b.Write(n[:])
	b.Write(s)
}

// PublicLine is the OpenSSH public key line "ssh-ed25519 AAAA…" of a release key.
func PublicLine(k keys.AKey) (string, error) {
	blob, err := pubBlob(k)
	if err != nil {
		return "", err
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob), nil
}

// SignSSH returns the armored sshsig signature of msg by s in the namespace (PROTOCOL.sshsig).
func SignSSH(s *keys.Signer, namespace string, msg []byte) ([]byte, error) {
	if namespace == "" {
		return nil, errors.New("installkit: empty namespace")
	}
	blob, err := pubBlob(s.Public())
	if err != nil {
		return nil, err
	}
	digest := sha512.Sum512(msg)
	var signed bytes.Buffer
	signed.WriteString("SSHSIG")
	putString(&signed, []byte(namespace))
	putString(&signed, nil)
	putString(&signed, []byte("sha512"))
	putString(&signed, digest[:])
	sig := s.Sign(signed.Bytes())

	var sigBlob bytes.Buffer
	putString(&sigBlob, []byte("ssh-ed25519"))
	putString(&sigBlob, sig)

	var out bytes.Buffer
	out.WriteString("SSHSIG")
	_ = binary.Write(&out, binary.BigEndian, uint32(1))
	putString(&out, blob)
	putString(&out, []byte(namespace))
	putString(&out, nil)
	putString(&out, []byte("sha512"))
	putString(&out, sigBlob.Bytes())

	enc := base64.StdEncoding.EncodeToString(out.Bytes())
	var arm strings.Builder
	arm.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for len(enc) > 70 {
		arm.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	arm.WriteString(enc + "\n-----END SSH SIGNATURE-----\n")
	return []byte(arm.String()), nil
}

// KeyBlock is the exact text between the BEGIN and END markers of install.sh: the sorted key
// lines and the key epoch. Its SHA-256 is the key fingerprint shown on the site and in the README.
func KeyBlock(ks []keys.AKey, epoch uint32) (string, error) {
	if epoch == 0 {
		return "", errors.New("installkit: the key epoch starts at 1")
	}
	if len(ks) < Threshold || len(ks) > maxKeys {
		return "", fmt.Errorf("installkit: %d release keys; need %d to %d", len(ks), Threshold, maxKeys)
	}
	lines := make([]string, 0, len(ks))
	seen := map[string]bool{}
	for _, k := range ks {
		l, err := PublicLine(k)
		if err != nil {
			return "", err
		}
		if seen[l] {
			return "", errors.New("installkit: a release key is listed twice")
		}
		seen[l] = true
		lines = append(lines, l)
	}
	sort.Strings(lines)
	return beginMarker + "RELEASE_KEYS='" + strings.Join(lines, "\n") + "'\nKEY_EPOCH=" + fmt.Sprint(epoch) + "\n" + endMarker, nil
}

// Fingerprint is the lower-case hex SHA-256 of KeyBlock.
func Fingerprint(ks []keys.AKey, epoch uint32) (string, error) {
	blk, err := KeyBlock(ks, epoch)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(blk))
	return hex.EncodeToString(sum[:]), nil
}

// Stamp puts the key set, the epoch and the version floor into a template of install.sh. The
// template carries the word PLACEHOLDER in its key block; a script that was stamped already is
// refused, so a key set is never swapped silently.
func Stamp(script string, ks []keys.AKey, epoch uint32, floor string) (string, error) {
	if _, ok := objects.ParseSemver(floor); !ok {
		return "", fmt.Errorf("installkit: floor %q is not X.Y.Z", floor)
	}
	blk, err := KeyBlock(ks, epoch)
	if err != nil {
		return "", err
	}
	if strings.Count(script, beginMarker) != 1 || strings.Count(script, endMarker) != 1 {
		return "", errors.New("installkit: the script must have exactly one key block")
	}
	i, j := strings.Index(script, beginMarker), strings.Index(script, endMarker)
	if j < i {
		return "", errors.New("installkit: the key block markers are in the wrong order")
	}
	if !strings.Contains(script[i:j], "PLACEHOLDER") {
		return "", errors.New("installkit: the script is stamped already; stamp the template")
	}
	if len(floorRe.FindAllString(script, -1)) != 1 {
		return "", errors.New("installkit: the script must have exactly one MIN_VERSION_FLOOR line")
	}
	out := script[:i] + blk + script[j+len(endMarker):]
	return floorRe.ReplaceAllLiteralString(out, "MIN_VERSION_FLOOR="+floor), nil
}

// Record renders the text release record from a release manifest. Only well-formed values reach a
// file that a shell script parses.
func Record(rel *pb.Release, epoch uint32) (string, error) {
	if epoch == 0 {
		return "", errors.New("installkit: the key epoch starts at 1")
	}
	ver, ok := objects.ParseSemver(rel.GetVersion())
	if !ok {
		return "", fmt.Errorf("installkit: version %q is not X.Y.Z", rel.GetVersion())
	}
	min := "0.0.0"
	if m := rel.GetMinVersion(); m != "" {
		mv, ok := objects.ParseSemver(m)
		if !ok {
			return "", fmt.Errorf("installkit: min_version %q is not X.Y.Z", m)
		}
		if objects.CompareSemver(mv, ver) > 0 {
			return "", errors.New("installkit: min_version is above the version of the release")
		}
		min = fmt.Sprintf("%d.%d.%d", mv[0], mv[1], mv[2])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "echofog-release 1\nversion %d.%d.%d\nmin_version %s\nkey_epoch %d\nreleased_at %d\n", ver[0], ver[1], ver[2], min, epoch, rel.GetReleasedAt())
	seen := map[string]bool{}
	for _, a := range rel.GetArtifacts() {
		if !platformRe.MatchString(a.GetPlatform()) || seen[a.GetPlatform()] {
			return "", fmt.Errorf("installkit: bad or repeated platform %q", a.GetPlatform())
		}
		seen[a.GetPlatform()] = true
		if len(a.GetSha256()) != sha256.Size || a.GetSize() == 0 || a.GetSize() > maxArtifactSize {
			return "", fmt.Errorf("installkit: artifact %s: hash or size", a.GetPlatform())
		}
		fmt.Fprintf(&b, "artifact %s %s %d\n", a.GetPlatform(), hex.EncodeToString(a.GetSha256()), a.GetSize())
	}
	return b.String(), nil
}

// SignRecord signs data in the namespace by every signer and returns the concatenated armored blocks.
// The caller decides how many distinct signers are enough (the tool insists on Threshold).
func SignRecord(data []byte, namespace string, signers ...*keys.Signer) ([]byte, error) {
	var out []byte
	for _, s := range signers {
		sig, err := SignSSH(s, namespace, data)
		if err != nil {
			return nil, err
		}
		out = append(out, sig...)
	}
	return out, nil
}

// Rotation renders the record, to be signed by the old keys, that announces the new key set.
func Rotation(next []keys.AKey, nextEpoch uint32) (string, error) {
	if nextEpoch < 2 {
		return "", errors.New("installkit: a rotation leads to epoch 2 or higher")
	}
	fp, err := Fingerprint(next, nextEpoch)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("echofog-rotation 1\nnew_epoch %d\nnew_keys_fingerprint %s\n", nextEpoch, fp), nil
}
