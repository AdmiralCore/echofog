//go:build linux && !android

// Package platform ties the drivers of the kill switch to the platform: on Linux the nftables table
// of the kill switch (a fixed table of the kernel), the tunnel device of v1 tunmode and the DNS
// guard over resolvectl, driven through an elevated helper (ks). Elsewhere the drivers are of
// another file.
//
// The firewall is nftables in the inet family: one table, two chains (output and input), policy
// drop. The script is applied with `nft -f` as one transaction, so a replacement never leaves the
// computer open for a moment and a killed process leaves the table in the kernel.
package platform

import (
	"context"
	"net"
	"os"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/ks"
)

// TableName is the nftables table of the kill switch. It is fixed and written in the operator
// guide: filters of an older run are found and removed by exactly this name.
const TableName = "echofog_ks"

// HelperFlag as os.Args[1] makes the executable run RunHelper.
const HelperFlag = ks.HelperFlag

// helperStartTimeout covers the prompt for root rights (polkit).
const helperStartTimeout = 2 * time.Minute

// Supported reports whether the kill switch has a driver here.
func Supported() bool { return true }

// Drivers is the application side.
func Drivers(exe string) *ks.Client {
	return ks.NewClient(ks.ClientConfig{
		Connect:   func(context.Context) (net.Conn, error) { return nil, tun.ErrUnsupported },
		Installed: func(context.Context) (bool, error) { return false, nil },
	})
}

// RunHelper is the main of the elevated helper.
func RunHelper(args []string) int { return 2 }

// FiltersStand reports whether the filters of the kill switch stand.
func FiltersStand(context.Context) bool { return false }

// Release removes the filters and restores DNS.
func Release(context.Context, string) error { return tun.ErrUnsupported }

// Report is what RemoveAll did.
type Report struct {
	Filters int
}

// RemoveAll removes the table and restores DNS.
func RemoveAll(context.Context) (Report, error) { return Report{}, tun.ErrUnsupported }

// ---- the drivers (stubs of the red commit) ------------------------------------------------------

type runner func(ctx context.Context, stdin, name string, args ...string) ([]byte, error)

type firewall struct {
	run    runner
	euid   func() int
	marker string
}

func newFirewall() *firewall { return &firewall{} }

func (f *firewall) Install(context.Context, []tun.Rule) error  { return nil }
func (f *firewall) Remove(context.Context) error               { return nil }
func (f *firewall) Installed(context.Context) (bool, error)    { return false, nil }
func (f *firewall) removeCounted(context.Context) (int, error) { return 0, nil }
func (f *firewall) setMarker(bool)                             {}
func renderRules([]tun.Rule, bool) string                      { return "" }
func orderRules([]tun.Rule) []tun.Rule                         { return nil }

type linkState struct {
	Name         string   `json:"name"`
	DNS          []string `json:"dns,omitempty"`
	Domains      []string `json:"domains,omitempty"`
	DefaultRoute bool     `json:"default_route"`
}

// dnsState is what Protect saves for Restore, also across a crash of the process.
type dnsState struct {
	Version int         `json:"version"`
	Links   []linkState `json:"links"`
}

func readState(string) ([]linkState, error) { return nil, os.ErrNotExist }

func writeState(string, []linkState) error { return nil }

type dnsGuard struct {
	run    runner
	euid   func() int
	state  string
	tunnel string
}

func newDNS() *dnsGuard { return &dnsGuard{} }

func (g *dnsGuard) Protect(context.Context, tun.DNSConfig) error  { return nil }
func (g *dnsGuard) Restore(context.Context) error                 { return nil }
func (g *dnsGuard) snapshot(context.Context) ([]linkState, error) { return nil, nil }
func parseStatus(string) []linkState                              { return nil }
func parseLinkValues(string) map[string][]string                  { return nil }

type tunnelController interface {
	Start(ctx context.Context, socksAddr string, bypassUDPPort int, holdFile string) error
	Stop()
	Running() bool
	SetOnExit(func(error))
}

type linuxTunnel struct {
	ctl         tunnelController
	ifaceExists func(string) bool
	events      chan tun.TunEvent
}

func newTunnel() *linuxTunnel { return &linuxTunnel{} }

func (t *linuxTunnel) Up(context.Context, tun.TunConfig) error { return nil }
func (t *linuxTunnel) Down(context.Context) error              { return nil }
func (t *linuxTunnel) Events() <-chan tun.TunEvent             { return t.events }
func (t *linuxTunnel) Alive(context.Context) bool              { return false }
func (t *linuxTunnel) exited(error)                            {}
