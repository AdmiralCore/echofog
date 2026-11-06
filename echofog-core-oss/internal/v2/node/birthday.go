package node

import "github.com/echofog/echofog-core/internal/v2/punch"

// BirthdayGates are the conditions of services.md §6.4.1 for one session: noise3 allowed to the peer,
// the feature negotiated, and the node's own modes and profile flag.
func BirthdayGates(p Profile, proxy, anonymous, noise3Allowed, negotiated bool) punch.Gates {
	return punch.Gates{}
}
