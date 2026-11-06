package app

import (
	"context"
	"fmt"
	"time"
)

// Mode returns the mode the node runs in; NextMode the saved one when it differs ("" if same).
func (a *App) Mode() Mode     { return "" }
func (a *App) NextMode() Mode { return "" }

// SetMode saves the mode for the next start.
func (a *App) SetMode(m Mode) error { return errNotImplemented }

// Joined is somebody who came in by an invitation of this node.
type Joined struct {
	Pass      string
	JoinedAt  time.Time
	Connected bool
}

// AccessStatus is the access screen: the mode, the contact link or why there is none.
type AccessStatus struct {
	Mode      Mode
	NextMode  Mode
	Link      string
	WebLink   string
	LinkError string
	Joined    []Joined
}

// AccessStatus reports the access screen.
func (a *App) AccessStatus(ctx context.Context) (AccessStatus, error) {
	return AccessStatus{}, errNotImplemented
}

// InviteRequest asks for an invitation. Days is the access term after the first entry (0 = no
// end), LinkHours how long the link can be opened (0 = default), Entries how many people can use it.
type InviteRequest struct {
	Days      float64
	LinkHours float64
	Entries   uint32
}

// Invitation is a fresh invitation. Its Link is a bearer secret: no formatter prints it.
type Invitation struct {
	Pass    string
	Link    string
	Web     string
	Expires time.Time
	Entries uint32
}

func (i Invitation) String() string {
	return fmt.Sprintf("app.Invitation{pass %s, link hidden}", i.Pass)
}
func (i Invitation) GoString() string { return i.String() }

// Invite mints an invitation of a hidden node.
func (a *App) Invite(ctx context.Context, r InviteRequest) (Invitation, error) {
	return Invitation{}, errNotImplemented
}

// PassView is a pass as the passes screen shows it.
type PassView struct {
	ID            string
	Kind          string // "invitation" | "personal"
	State         string // "active" | "used" | "expired" | "revoked"
	Created       time.Time
	LinkExpires   time.Time
	AccessExpires time.Time
	Entries       uint32
	MaxEntries    uint32
}

// Passes lists the passes, newest first.
func (a *App) Passes(ctx context.Context) ([]PassView, error) { return nil, errNotImplemented }

// ExtendPass lengthens the access term of a pass by days.
func (a *App) ExtendPass(ctx context.Context, id string, days float64) (PassView, error) {
	return PassView{}, errNotImplemented
}

// RevokePass revokes a pass.
func (a *App) RevokePass(ctx context.Context, id string) (PassView, error) {
	return PassView{}, errNotImplemented
}

// RotateAccessKeys rotates the masking keys of the node.
func (a *App) RotateAccessKeys(ctx context.Context) error { return errNotImplemented }
