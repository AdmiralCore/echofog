package node

import (
	"fmt"
	"net/netip"

	"github.com/echofog/echofog-core/internal/v2/identity"
)

// Platform is the runtime profile name (platforms.md §2). Everything it selects is local policy:
// the peer sees only legal v2 behaviour.
type Platform string

const (
	PlatformAndroid Platform = "android"
	PlatformIOS     Platform = "ios"
	PlatformDesktop Platform = "desktop"
	PlatformServer  Platform = "server"
)

// Role is a service role the node offers to others (platforms.md §2, registries.md §11).
type Role string

const (
	RoleRelay     Role = "relay"
	RoleExit      Role = "exit"
	RoleMailbox   Role = "mailbox"
	RoleDHTServer Role = "dht-server"
)

// Background is the app lifecycle state of the node (platforms.md §2).
type Background string

const (
	// BackgroundForeground is the normal state: sessions live as long as they are used.
	BackgroundForeground Background = "foreground"
	// BackgroundLimited keeps one session to a guard or mailbox relay, closes the others, and
	// relies on mailbox/2 FETCH with wait (T7.4).
	BackgroundLimited Background = "limited"
	// BackgroundSuspended closes every session with GOAWAY; on resume the app fetches from
	// mailboxes.
	BackgroundSuspended Background = "suspended"
)

// UDPMode is the udp capability flag (platforms.md §2). restricted disables GSO, recvmmsg, ECN and
// shrinks the noise3 padding range in the carrier; blocked leaves only TCP suites.
type UDPMode string

const (
	UDPFull       UDPMode = "full"
	UDPRestricted UDPMode = "restricted"
	UDPBlocked    UDPMode = "blocked"
)

// idlePaddingFeature is registries.md §5 feature 0x0003. A low_power node does not offer it
// (platforms.md §2).
const idlePaddingFeature uint32 = 0x0003

// Profile is the platform profile and the capability flags of platforms.md §2. The profile sets
// defaults; each field overrides one behaviour. Every field is local policy: no byte on the wire
// depends on it.
//
// The runtime acts on low_power (keepalive and idle-padding), mem_cap_mb (session limit), udp
// (blocked suites), local_addrs (carrier plan) and background. The remaining flags are carried for
// the tasks that consume them: carriers use protect, port_mapping, stun and udp=restricted; DHT,
// PEX, relay and mailbox use roles, metered and low_power; identity uses secret_store; the exit
// service uses tun_fd.
type Profile struct {
	Platform Platform

	// LowPower: 40–55 s keepalive instead of 12–24 s, no standby paths, no idle-padding, lazy
	// PEX, no background relaying (platforms.md §2).
	LowPower bool
	// Metered: no files/2 prefetch, no relaying for others, DHT republishing of own records only.
	Metered bool
	// Roles is the set of roles the node offers. Cards and service lists must list only these.
	Roles []Role
	// Background is the app lifecycle state.
	Background Background
	// TunFD is the VpnService / packet-flow descriptor, or −1 for no TUN. The zero value is
	// normalized to −1 by WithDefaults.
	TunFD int
	// Protect is the app's VPN bypass callback: every peer socket is passed to it before use
	// (platforms.md §1.1).
	Protect func(fd uintptr) error
	// LocalAddrs reports local addresses and network changes; it replaces net.Interfaces on
	// mobile (platforms.md §1.1).
	LocalAddrs func() []netip.Addr
	// PortMapping enables UPnP, NAT-PMP and PCP.
	PortMapping bool
	// Birthday enables birthday punching through symmetric NATs (flag birthday, platforms.md §2). It
	// is off by default on mobile and always off under LowPower.
	Birthday bool
	// Guard is set by the application in guard mode: no birthday punching (services.md §6.4.1).
	Guard bool
	// STUN enables active STUN. v2 never uses STUN on carrier sockets (wire.md §7.2).
	STUN bool
	// MemCapMB caps the memory the node may use; a positive value also selects the mobile
	// session limit (platforms.md §2, T4.3).
	MemCapMB int
	// UDP is the UDP capability mode.
	UDP UDPMode
	// SecretStore wraps and unwraps the at-rest key with the platform keystore (T4.1).
	SecretStore identity.Protector
}

// DefaultProfile returns the default flags of one platform (platforms.md §2, §4).
//
// Mobile defaults follow the conformance profile of platforms.md §4: no roles, udp=restricted,
// mem_cap_mb 128 (iOS extension 40), no STUN, no port mapping. low_power and metered are **off**:
// the platform layer sets them dynamically when the OS reports battery saver or a metered network
// (decisions.md T3.7b п.2); the profile never enables savings silently. A mobile node never sets
// role=server and never runs a public listener for strangers.
//
// A server offers dht-server, mailbox and relay: a server that offers nothing is not a server.
// The exit role is never on by default because it is a separate paid service with an ExitOffer
// (v1 owner decision); the operator adds it explicitly.
func DefaultProfile(p Platform) Profile {
	prof := Profile{
		Platform:   p,
		Background: BackgroundForeground,
		TunFD:      -1,
	}
	switch p {
	case PlatformAndroid:
		prof.MemCapMB, prof.UDP = 128, UDPRestricted
	case PlatformIOS:
		prof.MemCapMB, prof.UDP = 40, UDPRestricted
	case PlatformServer:
		prof.Roles = []Role{RoleDHTServer, RoleMailbox, RoleRelay}
		prof.UDP = UDPFull
	default:
		prof.Platform = PlatformDesktop
		prof.UDP = UDPFull
	}
	return prof
}

// WithDefaults completes the zero values of a hand-built profile. A zero Platform is a desktop
// node, an empty Background is foreground, an empty UDP mode follows the platform, a zero memory
// cap on mobile becomes the platform default, and TunFD 0 becomes −1 (no TUN).
func (p Profile) WithDefaults() Profile {
	if p.Platform == "" {
		p.Platform = PlatformDesktop
	}
	if p.Background == "" {
		p.Background = BackgroundForeground
	}
	if p.UDP == "" {
		if p.Mobile() {
			p.UDP = UDPRestricted
		} else {
			p.UDP = UDPFull
		}
	}
	if p.MemCapMB == 0 && p.Mobile() {
		if p.Platform == PlatformIOS {
			p.MemCapMB = 40
		} else {
			p.MemCapMB = 128
		}
	}
	if p.TunFD == 0 {
		p.TunFD = -1
	}
	return p
}

// Mobile reports whether the platform is a phone.
func (p Profile) Mobile() bool {
	return p.Platform == PlatformAndroid || p.Platform == PlatformIOS
}

// HasRole reports whether the node offers r.
func (p Profile) HasRole(r Role) bool {
	for _, have := range p.Roles {
		if have == r {
			return true
		}
	}
	return false
}

// Validate refuses unknown values instead of silently treating them as a default.
func (p Profile) Validate() error {
	switch p.Platform {
	case PlatformAndroid, PlatformIOS, PlatformDesktop, PlatformServer:
	default:
		return fmt.Errorf("%w: profile platform %q", ErrConfig, p.Platform)
	}
	switch p.Background {
	case BackgroundForeground, BackgroundLimited, BackgroundSuspended:
	default:
		return fmt.Errorf("%w: profile background %q", ErrConfig, p.Background)
	}
	switch p.UDP {
	case UDPFull, UDPRestricted, UDPBlocked:
	default:
		return fmt.Errorf("%w: profile udp %q", ErrConfig, p.UDP)
	}
	for _, r := range p.Roles {
		switch r {
		case RoleRelay, RoleExit, RoleMailbox, RoleDHTServer:
		default:
			return fmt.Errorf("%w: profile role %q", ErrConfig, r)
		}
	}
	if p.MemCapMB < 0 {
		return fmt.Errorf("%w: profile mem_cap_mb %d", ErrConfig, p.MemCapMB)
	}
	// tun_fd is −1 (no TUN) or a real descriptor above the standard streams: 0/1/2 are
	// stdin/stdout/stderr, and every other negative value is not a descriptor
	// (decisions.md T3.7b п.5).
	if p.TunFD != -1 && p.TunFD <= 2 {
		return fmt.Errorf("%w: profile tun_fd %d (want -1 or > 2)", ErrConfig, p.TunFD)
	}
	return nil
}
