package tun

import (
	"context"
	"errors"
	"net/netip"
)

// Errors of the drivers.
var (
	// ErrNoPrivilege: the process may not install firewall filters or create the device
	// (not elevated). The kill switch then refuses to start: no tunnel without it.
	ErrNoPrivilege = errors.New("tun: not enough privileges")
	// ErrUnsupported: no driver for this platform.
	ErrUnsupported = errors.New("tun: whole-computer mode is not supported here")
)

// Firewall installs the rules of the kill switch. Install replaces the whole set atomically and
// the rules persist after the process dies (WFP: persistent filters in the BFE; Linux: a table in
// the kernel). Installed answers from the firewall, not from memory, so a restart can find what
// an earlier process left.
type Firewall interface {
	Install(ctx context.Context, rules []Rule) error
	Remove(ctx context.Context) error
	Installed(ctx context.Context) (bool, error)
}

// TunConfig is what the tunnel device needs.
type TunConfig struct {
	Name     string
	Addrs    []netip.Prefix // IPv4 and IPv6 of the interface
	Resolver netip.Addr     // the fake DNS answering inside the tunnel
}

// TunEvent says the tunnel is gone: the helper died, the adapter was removed or the control
// connection dropped.
type TunEvent struct {
	Err error
}

// Tunnel is the TUN device with its helper.
type Tunnel interface {
	Up(ctx context.Context, cfg TunConfig) error
	Down(ctx context.Context) error
	// Events delivers one event when a running tunnel is lost; it is not used for Down. The driver
	// never closes the channel (the Switch stops watching a closed one).
	Events() <-chan TunEvent
	// Alive asks the device itself whether the tunnel is up. The Switch asks once per
	// Config.PollInterval, so a lost event (a full buffer, a missed notification) still ends in
	// StateHeld.
	Alive(ctx context.Context) bool
}

// DNSConfig is the DNS protection of the tunnel mode.
type DNSConfig struct {
	Tunnel   string
	Resolver netip.Addr
	// DisableSmartNameResolution is the Windows policy that stops the resolver from sending a query
	// to every interface's DNS server in parallel (the classic leak). Also the parallel A/AAAA
	// behaviour through other interfaces.
	DisableSmartNameResolution bool
}

// DNSGuard pins name resolution to the tunnel and flushes caches that hold real addresses.
type DNSGuard interface {
	Protect(ctx context.Context, cfg DNSConfig) error
	Restore(ctx context.Context) error
}
