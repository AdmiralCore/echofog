//go:build windows

// Package wintunnel is the tunnel device of the Windows kill switch (T10.6b): the Wintun adapter
// and the tun2socks helper of v1 (internal/network/tunmode), driven from the elevated process of
// the kill switch so that no second prompt for rights is needed.
package wintunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/echofog/echofog-core/internal/network/tunmode"
	"github.com/echofog/echofog-core/internal/v2/tun"
	"golang.org/x/sys/windows"
)

// controller is the part of *tunmode.Controller the driver uses.
type controller interface {
	Start(ctx context.Context, socksAddr string, bypassUDPPort int, holdFile string) error
	Stop()
	Running() bool
	SetOnExit(f func(error))
}

// tunnelLUID is the part of the firewall driver that learns the adapter.
type tunnelLUID interface {
	SetTunnelLUID(luid uint64) error
}

// Tunnel is the tun.Tunnel of Windows.
type Tunnel struct {
	ctl    controller
	fw     tunnelLUID
	luidOf func() (uint64, error)
	events chan tun.TunEvent

	mu       sync.Mutex
	up       bool
	stopping bool
}

// New returns the driver. fw learns the LUID of the adapter once it exists, so that the filters of
// the kill switch permit it and nothing else.
func New(fw tunnelLUID) *Tunnel {
	return &Tunnel{ctl: &tunmode.Controller{}, fw: fw, luidOf: adapterLUID, events: make(chan tun.TunEvent, 4)}
}

// adapterLUID finds the interface LUID of the EchoFog adapter.
func adapterLUID() (uint64, error) {
	ifc, err := net.InterfaceByName(tunmode.DeviceName)
	if err != nil {
		return 0, fmt.Errorf("wintunnel: the adapter %s: %w", tunmode.DeviceName, err)
	}
	row := windows.MibIfRow2{InterfaceIndex: uint32(ifc.Index)}
	if err := windows.GetIfEntry2Ex(windows.MibIfEntryNormal, &row); err != nil {
		return 0, fmt.Errorf("wintunnel: read the adapter: %w", err)
	}
	if row.InterfaceLuid == 0 {
		return 0, errors.New("wintunnel: the adapter has no LUID")
	}
	return row.InterfaceLuid, nil
}

func loopbackProxy(addr string) bool {
	ap, err := netip.ParseAddrPort(addr)
	return err == nil && ap.Addr().IsLoopback() && ap.Port() != 0
}

func (t *Tunnel) Up(ctx context.Context, cfg tun.TunConfig) error {
	return errors.New("not implemented")
}

// exited is the helper of v1 going away without being told to.
func (t *Tunnel) exited(err error) {
	t.mu.Lock()
	t.up = false
	stopping := t.stopping
	t.mu.Unlock()
	if stopping {
		return
	}
	if err == nil {
		err = errors.New("the tunnel helper exited")
	}
	select {
	case t.events <- tun.TunEvent{Err: err}:
	default:
	}
}

func (t *Tunnel) Down(ctx context.Context) error {
	return errors.New("not implemented")
}

func (t *Tunnel) Events() <-chan tun.TunEvent { return t.events }

// Alive is true while the helper runs and the adapter is there.
func (t *Tunnel) Alive(context.Context) bool {
	return false
}

var _ tun.Tunnel = (*Tunnel)(nil)
