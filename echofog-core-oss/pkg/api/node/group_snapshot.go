package node

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// Group snapshots (.fogzip). A member hands a newcomer a file instead of (or with) a link: the
// group's invitation and what its log holds (members, roles, channels, threads, messages), sealed
// with a password the member tells the newcomer apart. Importing joins by the invitation, as the
// link does, and takes the events into the log as history, so the newcomer reads the group at once,
// even while no member is reachable. Every event is signed by its author and checked on import:
// the file can be passed around, never changed. What it cannot do is let the newcomer in: the
// keys for new messages, the approval an invitation asks for, and calls still need a member.
//
//	"FOGZIP1\n" | salt (16) | nonce (24) | XChaCha20-Poly1305(gzip(JSON)), key = Argon2id(password, salt)

const (
	snapshotMagic    = "FOGZIP1\n"
	snapshotMaxBytes = 32 << 20 // events in one file
	snapshotMinPass  = 8
)

type groupSnapshot struct {
	V      int      `json:"v"`
	Title  string   `json:"title"`
	Invite string   `json:"invite"`
	At     int64    `json:"at"` // unix s
	Events [][]byte `json:"events"`
}

// GroupSnapshotInfo is what an import brought.
type GroupSnapshotInfo struct {
	Group  GroupDTO `json:"group"`
	Events int      `json:"events"` // new to this log
}

func snapshotKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, chacha20poly1305.KeySize)
}

// GroupSnapshotExport writes a group's snapshot to path, sealed with password.
func (n *NodeApi) GroupSnapshotExport(groupID, path, password string) error {
	if utf8.RuneCountInString(password) < snapshotMinPass {
		return fmt.Errorf("the password needs at least %d characters", snapshotMinPass)
	}
	f, c, err := n.roleLog(groupID)
	if err != nil {
		return err
	}
	gf, err := n.groupsFacade()
	if err != nil {
		return err
	}
	invite, err := n.quickInvite(groupID)
	if err != nil {
		return err
	}
	snap := groupSnapshot{V: 1, Title: gf.titleOf(groupID), Invite: invite, At: time.Now().Unix(),
		Events: f.log.Export(c, snapshotMaxBytes)}
	plain, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	if _, err := zw.Write(plain); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	aead, err := chacha20poly1305.NewX(snapshotKey(password, salt))
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := append([]byte(snapshotMagic), salt...)
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, zipped.Bytes(), []byte(snapshotMagic))
	return os.WriteFile(path, out, 0o600)
}

var errSnapshot = errors.New("not a group file, or the password is wrong")

// GroupSnapshotImport opens a snapshot: it joins the group by its invitation and takes its events
// in as history.
func (n *NodeApi) GroupSnapshotImport(path, password string) (GroupSnapshotInfo, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return GroupSnapshotInfo{}, err
	}
	snap, err := openSnapshot(raw, password)
	if err != nil {
		return GroupSnapshotInfo{}, err
	}
	g, err := n.JoinGroup(snap.Invite)
	if err != nil {
		return GroupSnapshotInfo{}, err
	}
	f, err := n.convsFacade()
	if err != nil {
		return GroupSnapshotInfo{Group: g}, err
	}
	c := convlog.Group(g.ID)
	_ = f.log.Open(c)
	added := f.log.ImportHistory(c, snap.Events)
	f.groupHistoryArrived(g.ID)
	return GroupSnapshotInfo{Group: g, Events: added}, nil
}

func openSnapshot(raw []byte, password string) (*groupSnapshot, error) {
	if !bytes.HasPrefix(raw, []byte(snapshotMagic)) {
		return nil, errSnapshot
	}
	body := raw[len(snapshotMagic):]
	if len(body) < 16+chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead {
		return nil, errSnapshot
	}
	salt, nonce, sealed := body[:16], body[16:16+chacha20poly1305.NonceSizeX], body[16+chacha20poly1305.NonceSizeX:]
	aead, err := chacha20poly1305.NewX(snapshotKey(password, salt))
	if err != nil {
		return nil, err
	}
	zipped, err := aead.Open(nil, nonce, sealed, []byte(snapshotMagic))
	if err != nil {
		return nil, errSnapshot
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		return nil, errSnapshot
	}
	plain, err := io.ReadAll(io.LimitReader(zr, 4*snapshotMaxBytes))
	if err != nil {
		return nil, errSnapshot
	}
	var snap groupSnapshot
	if json.Unmarshal(plain, &snap) != nil || snap.V != 1 || !strings.HasPrefix(snap.Invite, inviteScheme2) {
		return nil, errSnapshot
	}
	return &snap, nil
}
