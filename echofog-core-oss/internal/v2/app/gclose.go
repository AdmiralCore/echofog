package app

import (
	"context"
	"errors"
)

// ErrGroupClosed reports a write to a group its owner closed.
var ErrGroupClosed = errors.New("app: the group is closed")

// GroupClose closes the group for everybody (the owner only): irreversible, the members keep an archive
// to read. Copies the members hold do not go away.
func (a *App) GroupClose(ctx context.Context, id string) error { return errors.New("not built") }
