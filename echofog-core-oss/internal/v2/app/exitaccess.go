package app

import (
	"errors"

	"github.com/echofog/echofog-core/internal/v2/store"
)

// Limits of the queue of clients waiting for the owner's confirmation.
const (
	ExitPendingMax       = 16
	ExitPendingTTL       = 3600 // seconds
	ExitPendingPerSource = 2
)

// ErrExitNoEntry reports an id that is not in the queue or in the confirmed list.
var ErrExitNoEntry = errors.New("app: no such client")

// ExitPendingView is a client that tried and was turned away; the owner may confirm it.
type ExitPendingView struct {
	ID    string
	Since int64
	Last  int64
	Tries int
}

// ExitApprovedView is a confirmed client.
type ExitApprovedView struct {
	ID    string
	Label string
	Since int64
}

// ExitAccess decides who the exit serves. The unit of admission is the identity a client uses for
// this exit; the messenger identity is never seen. Confirmed clients are remembered in the store;
// the queue and the open access live in memory only.
type ExitAccess struct{}

// NewExitAccess returns the access rules over a store (nil: in memory).
func NewExitAccess(clk Clock, kv store.KV) *ExitAccess { return &ExitAccess{} }

// Admit says whether the client is served now. It has no side effects.
func (x *ExitAccess) Admit(node [32]byte) bool { return true }

// Refused records a client that was turned away: it waits for the owner, within the limits.
func (x *ExitAccess) Refused(node [32]byte, source string) {}

// Pending lists the waiting clients, oldest first, without those that waited out the TTL.
func (x *ExitAccess) Pending() []ExitPendingView { return nil }

// Approved lists the confirmed clients.
func (x *ExitAccess) Approved() []ExitApprovedView { return nil }

// Approve confirms a waiting client under label.
func (x *ExitAccess) Approve(id, label string) error { return errors.New("not implemented") }

// Reject removes a waiting client.
func (x *ExitAccess) Reject(id string) error { return errors.New("not implemented") }

// Revoke forgets a confirmed client.
func (x *ExitAccess) Revoke(id string) error { return errors.New("not implemented") }

// AllowAny opens the exit to anyone for ttl seconds and returns the end (unix seconds).
func (x *ExitAccess) AllowAny(ttl int64) int64 { return 0 }

// RevokeAny closes the open access.
func (x *ExitAccess) RevokeAny() {}

// AnyUntil is the end of the open access, 0 when it is closed or over.
func (x *ExitAccess) AnyUntil() int64 { return 0 }
