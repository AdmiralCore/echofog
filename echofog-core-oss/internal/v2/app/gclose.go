package app

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/services/convlog"
)

// ErrGroupClosed reports a write to a group its owner closed.
var ErrGroupClosed = errors.New("app: the group is closed")

// GroupClose closes the group for everybody (the owner only): irreversible, the members keep an archive
// to read, nobody writes, nobody is let in. Copies the members hold do not go away. Closing a group that is
// closed already changes nothing.
func (a *App) GroupClose(ctx context.Context, id string) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	l := cv.Log()
	if l.Closed(c) {
		return nil
	}
	raw, _, err := l.AppendClose(c)
	if err != nil {
		if errors.Is(err, convlog.ErrClosed) {
			return nil
		}
		return rightErr(err)
	}
	a.publishGroup(ctx, cv, c, raw)
	return nil
}

// noteClosed tells the screen once that a group was closed by its owner: the members are told, the
// owner who did it is not.
func (a *App) noteClosed(c convlog.Conv, self bool) {
	a.gev.mu.Lock()
	if a.gev.closedTold == nil {
		a.gev.closedTold = map[string]bool{}
	}
	told := a.gev.closedTold[c.Group]
	a.gev.closedTold[c.Group] = true
	a.gev.mu.Unlock()
	if told || self {
		return
	}
	a.emitGroup(GroupEvent{Kind: "removed", Group: c.Group, Reason: "deleted"})
}
