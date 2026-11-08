package app

import (
	"context"
	"errors"
)

// Group snapshots (.fogzip), protocol 2 (T11.3e).

var (
	// ErrSnapshotPassword reports a password that is too short.
	ErrSnapshotPassword = errors.New("app: the snapshot password is too short")
	// ErrSnapshotFile reports a file that is not a group snapshot, or a wrong password: one error for both.
	ErrSnapshotFile = errors.New("app: not a group file, or the password is wrong")
	// ErrSnapshotVersion reports a snapshot made by another version of the protocol (the v1 file).
	ErrSnapshotVersion = errors.New("app: the group file was made by another version")
)

// GroupSnapshotExport writes the group's snapshot to path, sealed with password.
func (a *App) GroupSnapshotExport(ctx context.Context, id, path, password string) error {
	return errors.New("not built")
}

// GroupSnapshotImport opens a snapshot: it joins the group by the invitation inside and takes the
// signed events in as history. It returns the group and how many events were new.
func (a *App) GroupSnapshotImport(ctx context.Context, path, password string) (GroupInfo, int, error) {
	return GroupInfo{}, 0, errors.New("not built")
}

// snapshotDoc is what a snapshot holds.
type snapshotDoc struct {
	V      int      `json:"v"`
	Title  string   `json:"title"`
	Invite string   `json:"invite"`
	At     int64    `json:"at"` // unix s
	Events [][]byte `json:"events"`
}

func sealSnapshot(d *snapshotDoc, password string) ([]byte, error) {
	return nil, errors.New("not built")
}

func openSnapshot(raw []byte, password string) (*snapshotDoc, error) {
	return nil, errors.New("not built")
}
