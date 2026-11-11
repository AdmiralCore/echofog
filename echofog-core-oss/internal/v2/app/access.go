package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/node"
)

const (
	keyMode = "mode"

	maxTermDays  = 3650
	maxLinkHours = 24 * 30
	maxEntries   = 1000
)

// Mode returns the mode the node runs in.
func (a *App) Mode() Mode { return a.cfg.Mode }

// NextMode returns the mode saved for the next start when it differs from the running one.
func (a *App) NextMode() Mode {
	v, err := a.cfg.Store.Get([]byte(keyMode))
	if err != nil {
		return ""
	}
	if m := Mode(v); m.valid() && m != a.cfg.Mode {
		return m
	}
	return ""
}

// SetMode saves the mode for the next start: the card and the listeners of a node are built once,
// so a change applies after a restart.
func (a *App) SetMode(m Mode) error {
	if !m.valid() {
		return ErrMode
	}
	return a.cfg.Store.Put([]byte(keyMode), []byte(m))
}

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

// AccessStatus reports the access screen. A hidden node has no standing link: people come in by
// invitations (Invite).
func (a *App) AccessStatus(ctx context.Context) (AccessStatus, error) {
	st := AccessStatus{Mode: a.cfg.Mode, NextMode: a.NextMode()}
	if a.cfg.Mode == ModePublic {
		l, err := a.cfg.Links.ContactLink(ctx)
		if err != nil {
			st.LinkError = UserMessage(err)
		} else {
			st.Link, st.WebLink = l.URL, l.Web
		}
	}
	list, err := a.cfg.Runtime.AccessList(ctx)
	if err != nil && !errors.Is(err, node.ErrNoAccess) {
		return st, err // a node without a listener has no access manager: nobody joined by an invitation
	}
	live := map[[32]byte]bool{}
	for _, s := range a.cfg.Runtime.Status().Sessions {
		live[s.Device] = true
	}
	for _, in := range list {
		if in.Kind == access.Personal && in.State == access.Active {
			st.Joined = append(st.Joined, Joined{Pass: in.ID.String(), JoinedAt: in.Created, Connected: live[in.Device]})
		}
	}
	return st, nil
}

// InviteRequest asks for an invitation. Days is the access term after the first entry (0 = no
// end), LinkHours how long the link can be opened (0 = default), Entries how many people can use it
// (0 = one).
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

func inRange(v, max float64) bool { return !math.IsNaN(v) && v >= 0 && v <= max }

func hours(h float64) time.Duration { return time.Duration(h * float64(time.Hour)) }

// Invite mints an invitation of a hidden node.
func (a *App) Invite(ctx context.Context, r InviteRequest) (Invitation, error) {
	if a.cfg.Mode != ModeHidden {
		return Invitation{}, ErrNotHidden
	}
	if !inRange(r.Days, maxTermDays) || !inRange(r.LinkHours, maxLinkHours) || r.Entries > maxEntries {
		return Invitation{}, ErrRequest
	}
	inv, err := a.cfg.Links.Invite(ctx, access.MintOptions{
		AccessTTL: hours(r.Days * 24), LinkTTL: hours(r.LinkHours), MaxEntries: r.Entries,
	})
	if err != nil {
		return Invitation{}, err
	}
	return Invitation{Pass: inv.PassID.String(), Link: inv.URL, Web: inv.Web, Expires: inv.Expires, Entries: inv.MaxEntries}, nil
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

func passView(in access.Info) PassView {
	return PassView{ID: in.ID.String(), Kind: in.Kind.String(), State: in.State.String(), Created: in.Created,
		LinkExpires: in.LinkExpires, AccessExpires: in.AccessExpires, Entries: in.Entries, MaxEntries: in.MaxEntries}
}

// Passes lists the passes, newest first.
func (a *App) Passes(ctx context.Context) ([]PassView, error) {
	list, err := a.cfg.Runtime.AccessList(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Created.After(list[j].Created) })
	out := make([]PassView, 0, len(list))
	for _, in := range list {
		out = append(out, passView(in))
	}
	return out, nil
}

func parsePass(id string) (access.PassID, error) {
	var p access.PassID
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != len(p) {
		return p, ErrNoPass
	}
	copy(p[:], b)
	return p, nil
}

func passErr(err error) error {
	if errors.Is(err, access.ErrNotFound) {
		return ErrNoPass
	}
	return err
}

// ExtendPass lengthens the access term of a pass by days.
func (a *App) ExtendPass(ctx context.Context, id string, days float64) (PassView, error) {
	p, err := parsePass(id)
	if err != nil {
		return PassView{}, err
	}
	if !inRange(days, maxTermDays) || days == 0 {
		return PassView{}, ErrRequest
	}
	in, err := a.cfg.Runtime.ExtendAccess(ctx, p, hours(days*24))
	if err != nil {
		return PassView{}, passErr(err)
	}
	return passView(in), nil
}

// RevokePass revokes a pass.
func (a *App) RevokePass(ctx context.Context, id string) (PassView, error) {
	p, err := parsePass(id)
	if err != nil {
		return PassView{}, err
	}
	res, err := a.cfg.Runtime.RevokeAccess(ctx, p)
	if err != nil {
		return PassView{}, passErr(err)
	}
	return passView(res.Pass), nil
}

// RotateAccessKeys rotates the masking keys of the node.
func (a *App) RotateAccessKeys(ctx context.Context) error {
	_, err := a.cfg.Runtime.RotateAccessKeys(ctx)
	return err
}
