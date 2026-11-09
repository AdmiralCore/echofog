package app

import (
	"context"

	"github.com/echofog/echofog-core/internal/v2/files"
)

// AttOfForTest exposes the rule attOf decides an offered file in a group by, without a transfer
// record in the way: a test can ask the log itself (T11.3e-1 Т-2).
func AttOfForTest(a *App, ctx context.Context, from [32]byte, o files.Offer) (group, channel, thread string, found, ok bool) {
	pl, found, ok := a.attOf(ctx, from, o)
	return pl.group, pl.channel, pl.thread, found, ok
}

// SnapshotDoc is the content of a group snapshot, for the tests that seal and open one themselves.
type SnapshotDoc = snapshotDoc

// OpenSnapshotForTest opens a sealed snapshot.
func OpenSnapshotForTest(raw []byte, password string) (*SnapshotDoc, error) {
	return openSnapshot(raw, password)
}

// SealSnapshotForTest seals a snapshot.
func SealSnapshotForTest(d *SnapshotDoc, password string) ([]byte, error) {
	return sealSnapshot(d, password)
}

// UnwebInviteForTest turns the web form of an invitation back into the app's.
func UnwebInviteForTest(s string) string { return unwebInvite(s) }
