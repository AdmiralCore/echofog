//go:build windows

package winwfp

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/tailscale/wf"
	"golang.org/x/sys/windows"
)

// engine is the part of *wf.Session the driver uses; tests replace it.
type engine interface {
	Providers() ([]*wf.Provider, error)
	AddProvider(*wf.Provider) error
	DeleteProvider(wf.ProviderID) error
	Sublayers(...wf.ProviderID) ([]*wf.Sublayer, error)
	AddSublayer(*wf.Sublayer) error
	DeleteSublayer(wf.SublayerID) error
	Rules() ([]*wf.Rule, error)
	AddRule(*wf.Rule) error
	DeleteRule(wf.RuleID) error
	Close() error
}

// Compile turns rules into WFP filters (not yet added). luid is the interface LUID of the tunnel
// adapter, 0 while there is none: rules for the tunnel are then left out, which keeps the
// computer closed. newID makes the key of each filter.
func Compile(rules []tun.Rule, luid uint64, newID func() windows.GUID) ([]*wf.Rule, error) {
	return nil, errors.New("not implemented")
}

// Firewall is the tun.Firewall of Windows.
type Firewall struct {
	open  func() (engine, error)
	newID func() windows.GUID
}

// NewFirewall returns the driver over the real Base Filtering Engine. It needs administrator
// rights for Install and Remove.
func NewFirewall() *Firewall { return &Firewall{} }

// SetTunnelLUID tells the driver which adapter is the tunnel; the installed rules are replaced with
// ones that permit it. Call it after the adapter exists (and with 0 after it is gone).
func (f *Firewall) SetTunnelLUID(luid uint64) error { return errors.New("not implemented") }

func (f *Firewall) Install(ctx context.Context, rules []tun.Rule) error {
	return errors.New("not implemented")
}

func (f *Firewall) Remove(ctx context.Context) error { return errors.New("not implemented") }

func (f *Firewall) Installed(ctx context.Context) (bool, error) {
	return false, errors.New("not implemented")
}

// RemoveAll deletes every object of the kill switch and returns how many filters it removed. It is
// the manual removal (efv2 killswitch remove).
func RemoveAll() (int, error) { return 0, errors.New("not implemented") }

func removeAll(e engine) (int, error) { return 0, errors.New("not implemented") }
