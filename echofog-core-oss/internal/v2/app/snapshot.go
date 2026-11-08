package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// Group snapshots (.fogzip), protocol 2 (T11.3e). A member hands a newcomer a file instead of (or with)
// a link: the group's invitation and the signed events of its log (members, roles, channels, threads,
// messages), sealed with a password the member tells the newcomer apart. Importing joins by the
// invitation, as the link does, and takes every event in as history, weighed as one that a member sends
// by sync: a changed event, or one nobody in the group may write, is dropped. The file can be passed
// around, never changed. It cannot let the newcomer in: the group's members decide that, as with a link.
//
//	"FOGZIP2\n" | salt (16) | nonce (24) | XChaCha20-Poly1305(gzip(JSON)), key = Argon2id(password, salt)
//
// The file of v1 ("FOGZIP1\n") is another format and is not taken: ErrSnapshotVersion, for the screen
// "made by another version" (T11.5).

var (
	// ErrSnapshotPassword reports a password that is too short.
	ErrSnapshotPassword = errors.New("app: the snapshot password is too short")
	// ErrSnapshotFile reports a file that is not a group snapshot, or a wrong password: one error for both.
	ErrSnapshotFile = errors.New("app: not a group file, or the password is wrong")
	// ErrSnapshotVersion reports a snapshot made by another version of the protocol (the v1 file).
	ErrSnapshotVersion = errors.New("app: the group file was made by another version")
)

const (
	snapMagic      = "FOGZIP2\n"
	snapMagicStem  = "FOGZIP"
	snapMinPass    = 8
	snapEventBytes = 32 << 20 // the events of one file
	snapMaxFile    = 48 << 20
	snapMaxPlain   = 96 << 20 // what the file may unpack to
	snapMaxEvents  = 200000
	snapSalt       = 16
	snapInviteTTL  = 7 * 24 * time.Hour
	snapInviteUses = 10
	snapVersion    = 2
)

// snapshotDoc is what a snapshot holds.
type snapshotDoc struct {
	V      int      `json:"v"`
	Title  string   `json:"title"`
	Invite string   `json:"invite"`
	At     int64    `json:"at"` // unix s
	Events [][]byte `json:"events"`
}

func snapshotKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, chacha20poly1305.KeySize)
}

func sealSnapshot(d *snapshotDoc, password string) ([]byte, error) {
	plain, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	salt := make([]byte, snapSalt)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(snapshotKey(password, salt))
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(snapMagic), salt...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, zipped.Bytes(), []byte(snapMagic)), nil
}

func openSnapshot(raw []byte, password string) (*snapshotDoc, error) {
	if !bytes.HasPrefix(raw, []byte(snapMagic)) {
		if bytes.HasPrefix(raw, []byte(snapMagicStem)) && len(raw) > len(snapMagic) && raw[len(snapMagic)-1] == '\n' {
			return nil, ErrSnapshotVersion // v1 or a later one: another format
		}
		return nil, ErrSnapshotFile
	}
	body := raw[len(snapMagic):]
	if len(body) < snapSalt+chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead {
		return nil, ErrSnapshotFile
	}
	salt, nonce, sealed := body[:snapSalt], body[snapSalt:snapSalt+chacha20poly1305.NonceSizeX], body[snapSalt+chacha20poly1305.NonceSizeX:]
	aead, err := chacha20poly1305.NewX(snapshotKey(password, salt))
	if err != nil {
		return nil, ErrSnapshotFile
	}
	zipped, err := aead.Open(nil, nonce, sealed, []byte(snapMagic))
	if err != nil {
		return nil, ErrSnapshotFile
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		return nil, ErrSnapshotFile
	}
	plain, err := io.ReadAll(io.LimitReader(zr, snapMaxPlain+1))
	if err != nil || len(plain) > snapMaxPlain {
		return nil, ErrSnapshotFile
	}
	var d snapshotDoc
	if json.Unmarshal(plain, &d) != nil || d.V != snapVersion || len(d.Events) > snapMaxEvents {
		return nil, ErrSnapshotFile
	}
	return &d, nil
}

// GroupSnapshotExport writes the group's snapshot to path, sealed with password. It makes an invitation
// for the newcomers (a week, ten uses), so it needs the right to make one.
func (a *App) GroupSnapshotExport(ctx context.Context, id, path, password string) error {
	if utf8.RuneCountInString(password) < snapMinPass {
		return ErrSnapshotPassword
	}
	_, cv, c, r, err := a.grp(id)
	if err != nil {
		return err
	}
	inv, err := a.GroupInviteCreate(ctx, id, InviteSpec{TTL: snapInviteTTL, Uses: snapInviteUses, Label: "snapshot"})
	if err != nil {
		return err
	}
	doc := &snapshotDoc{V: snapVersion, Title: r.Genesis.Title, Invite: inv.Link, At: a.cfg.Clock.Now().Unix(),
		Events: cv.Log().Export(c, snapEventBytes)}
	sealed, err := sealSnapshot(doc, password)
	if err != nil {
		return err
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// GroupSnapshotImport opens a snapshot: it joins the group by the invitation inside and takes the
// signed events in as history. It returns the group and how many events were new.
func (a *App) GroupSnapshotImport(ctx context.Context, path, password string) (GroupInfo, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return GroupInfo{}, 0, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, snapMaxFile+1))
	if err != nil {
		return GroupInfo{}, 0, err
	}
	if len(raw) > snapMaxFile {
		return GroupInfo{}, 0, ErrSnapshotFile
	}
	doc, err := openSnapshot(raw, password)
	if err != nil {
		return GroupInfo{}, 0, err
	}
	if strings.TrimSpace(doc.Invite) == "" {
		return GroupInfo{}, 0, ErrSnapshotFile
	}
	g, err := a.JoinGroup(ctx, doc.Invite)
	if err != nil {
		return GroupInfo{}, 0, err
	}
	_, cv, err := a.groupEnv()
	if err != nil {
		return g, 0, err
	}
	c := convlog.Group(g.ID)
	if err := cv.Log().Open(c); err != nil {
		return g, 0, err
	}
	n := cv.Log().ImportHistory(c, doc.Events)
	if info, err := a.GroupInfoOf(g.ID); err == nil {
		g = info
	}
	return g, n, nil
}
