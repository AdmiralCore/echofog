// Package route chooses the path a call's media datagrams take (T9.3, services.md 9, wire.md
// 8.4): the policy network.call_route = auto | direct | relay.
//
// Media is confidential and authenticated end to end by the call layer (media v3), and on a relay
// path every datagram is additionally a SEALED datagram of the session, so a relay carries
// ciphertext it cannot read or splice. The policy is about which kind of path a participant
// accepts, never about trust in the relay.
package route

import (
	"errors"
	"fmt"
	"strings"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Route is the call_route setting.
type Route uint8

const (
	// Auto prefers a direct path and uses a relay path only when there is no direct one.
	Auto Route = iota
	// Direct sends media on direct paths only. With none, media is not sent.
	Direct
	// Relay sends media on relay paths only (the peer never learns this node's address from the
	// media path). With none, media is not sent: it never falls back to a direct path.
	Relay
)

// ErrNoPath: the policy allows no live path to the peer.
var ErrNoPath = errors.New("call route: no path the route allows")

// ErrBadRoute: an unknown call_route value.
var ErrBadRoute = errors.New("call route: unknown value")

func (r Route) String() string {
	switch r {
	case Auto:
		return "auto"
	case Direct:
		return "direct"
	case Relay:
		return "relay"
	}
	return fmt.Sprintf("route(%d)", uint8(r))
}

// Parse reads a call_route setting. The empty string means auto.
func Parse(s string) (Route, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return Auto, nil
	case "direct":
		return Direct, nil
	case "relay":
		return Relay, nil
	}
	return Auto, fmt.Errorf("%w: %q", ErrBadRoute, s)
}

// Link is the part of a session the policy needs. *mux.Session satisfies it.
type Link interface {
	PathKinds() map[mux.PathKind]int
	SendDatagramVia(kind mux.PathKind, typ uint64, payload []byte) error
}

// Choose returns the kind of path the policy picks given the live paths, or ErrNoPath.
func Choose(r Route, kinds map[mux.PathKind]int) (mux.PathKind, error) {
	direct, relay := kinds[mux.PathDirect] > 0, kinds[mux.PathRelay] > 0
	switch r {
	case Auto:
		switch {
		case direct:
			return mux.PathDirect, nil
		case relay:
			return mux.PathRelay, nil
		}
	case Direct:
		if direct {
			return mux.PathDirect, nil
		}
	case Relay:
		if relay {
			return mux.PathRelay, nil
		}
	default:
		return 0, fmt.Errorf("%w: %d", ErrBadRoute, uint8(r))
	}
	return 0, ErrNoPath
}

// Send sends one datagram on the path kind the policy picks and reports the kind used. The
// choice and the send are not atomic: a path that dies in between makes SendDatagramVia fail
// with mux.ErrNoPathKind, and the datagram is lost (datagrams are best effort), never sent on
// another kind.
func Send(r Route, l Link, typ uint64, payload []byte) (mux.PathKind, error) {
	k, err := Choose(r, l.PathKinds())
	if err != nil {
		return 0, err
	}
	if err := l.SendDatagramVia(k, typ, payload); err != nil {
		return 0, err
	}
	return k, nil
}

// OnlyRelayed reports whether the live paths are relay paths only: the peer is reached through a
// relay and not directly. A session with no live path is not "relayed".
func OnlyRelayed(kinds map[mux.PathKind]int) bool {
	return kinds[mux.PathRelay] > 0 && kinds[mux.PathDirect] == 0
}
