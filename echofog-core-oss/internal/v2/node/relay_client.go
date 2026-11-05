package node

import "github.com/echofog/echofog-core/internal/v2/punch"

// PunchNetwork supplies the datagram side of hole punching for one session (T6.2): the addresses to
// offer, the junk sender on the noise3 listener's socket and the noise3 Init dialer.
type PunchNetwork interface {
	Net(peer DeviceID, role punch.Role) punch.Net
}
