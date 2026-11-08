package node

import (
	"errors"
	"net/netip"
)

// ExitOptions is how a node that is not an exit by profile (a desktop) offers one for a while (T10.9).
type ExitOptions struct {
	// Access admits the clients by node; nil admits everybody. It is asked for every new stream.
	Admit   func(node [32]byte, remote netip.AddrPort) bool
	Refused func(node [32]byte, remote netip.AddrPort)
	// MaxMbps limits what one client may take, in megabits a second; 0 keeps the default.
	MaxMbps int
	// AllowPrivateTargets lets clients reach private addresses: tests and closed deployments only.
	AllowPrivateTargets bool
}

// ExitClient is a client with streams open.
type ExitClient struct {
	Node    [32]byte
	Streams int
}

// ExitStatus is what the hosted exit has done.
type ExitStatus struct {
	Enabled           bool
	Accepted, Refused uint64
	Bytes             uint64
	RefusedBy         map[string]uint64
	Clients           []ExitClient
}

// ErrExitStatic reports an exit that the profile already carries: it cannot be switched.
var ErrExitStatic = errors.New("node: the exit role is part of the profile")

// EnableExit starts exit/2 and puts the exit offer on the cards, without a restart.
func (r *Runtime) EnableExit(opt ExitOptions) error { return errors.New("not implemented") }

// DisableExit takes the offer off the cards, stops exit/2, refuses new streams and closes the live ones.
func (r *Runtime) DisableExit() {}

// RecheckExit closes the open streams of the clients that are no longer admitted.
func (r *Runtime) RecheckExit() {}

// ExitStatus reports the hosted exit; the zero value when it is off.
func (r *Runtime) ExitStatus() ExitStatus { return ExitStatus{} }
