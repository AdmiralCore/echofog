package app

import (
	"context"
	"errors"
	"io"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// ExitLink is a connection to one exit (*exit.Client): a dialer of tunnels the local proxy hands out,
// closed when the VPN stops.
type ExitLink interface {
	DialExitStream(ctx context.Context, target string) (io.ReadWriteCloser, error)
	Close() error
}

// ExitDialer connects to the exit whose card it is given.
type ExitDialer interface {
	Dial(ctx context.Context, card *objects.Card) (ExitLink, error)
}

// VPN errors.
var (
	// ErrVPNRunning reports a start while the VPN runs.
	ErrVPNRunning = errors.New("app: the VPN is running")
	// ErrNotExit reports a link whose node offers no exit.
	ErrNotExit = errors.New("app: the node is no exit")
	// ErrVPNOff reports a VPN call on an app built without an exit dialer.
	ErrVPNOff = errors.New("app: the VPN is not available")
	// ErrNoExit reports an exit that is not in the list.
	ErrNoExit = errors.New("app: no such exit")
)

// VPNStatus is the state of the VPN (stub).
type VPNStatus struct {
	Running    bool
	Listen     string
	Exit       string
	Active     int64
	Rx, Tx     uint64
	ExitState  string
	Refused    uint64
	RefusedBy  map[string]uint64
	LastReason string
	LastHost   string
	LastAt     int64
}

// VPNStart starts the local proxy through the exit a link names (stub).
func (a *App) VPNStart(ctx context.Context, link string) (VPNStatus, error) {
	return VPNStatus{}, errors.New("app: not built")
}

// VPNStop stops the VPN (stub).
func (a *App) VPNStop() error { return errors.New("app: not built") }

// VPNStatus reports the VPN (stub).
func (a *App) VPNStatus() VPNStatus { return VPNStatus{} }

// SavedExit is an exit the user has used or marked (stub).
type SavedExit struct {
	Node, Name       string
	Favorite, Active bool
	Sessions         int
	LastUsed         int64
	Rx, Tx, Refused  uint64
	RefusedBy        map[string]uint64
}

// VPNSession is one run of the VPN (stub).
type VPNSession struct {
	Node, EndReason    string
	StartedAt, EndedAt int64
	Rx, Tx, Refused    uint64
	RefusedBy          map[string]uint64
}

func (a *App) VPNExits() []SavedExit                  { return nil }
func (a *App) VPNHistory() []VPNSession               { return nil }
func (a *App) VPNName(node, name string) error        { return errors.New("app: not built") }
func (a *App) VPNFavorite(node string, on bool) error { return errors.New("app: not built") }
func (a *App) VPNForget(node string) error            { return errors.New("app: not built") }
func (a *App) VPNClearHistory() error                 { return errors.New("app: not built") }
func (a *App) VPNStartSaved(ctx context.Context, node string) (VPNStatus, error) {
	return VPNStatus{}, errors.New("app: not built")
}
