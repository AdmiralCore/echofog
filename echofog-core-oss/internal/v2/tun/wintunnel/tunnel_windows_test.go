//go:build windows

package wintunnel

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
)

type fakeCtl struct {
	running bool
	onExit  func(error)
	started struct {
		socks string
		udp   int
		hold  string
	}
	failStart error
	stops     int
}

func (c *fakeCtl) Start(_ context.Context, socks string, udp int, hold string) error {
	if c.failStart != nil {
		return c.failStart
	}
	c.started.socks, c.started.udp, c.started.hold = socks, udp, hold
	c.running = true
	return nil
}
func (c *fakeCtl) Stop()                   { c.running = false; c.stops++ }
func (c *fakeCtl) Running() bool           { return c.running }
func (c *fakeCtl) SetOnExit(f func(error)) { c.onExit = f }

type fakeLUID struct{ calls []uint64 }

func (f *fakeLUID) SetTunnelLUID(l uint64) error { f.calls = append(f.calls, l); return nil }

func newTunnel(ctl *fakeCtl, fw *fakeLUID, luid uint64, present bool) *Tunnel {
	return &Tunnel{ctl: ctl, fw: fw, luidOf: func() (uint64, error) {
		if !present {
			return 0, errors.New("no adapter")
		}
		return luid, nil
	}, events: make(chan tun.TunEvent, 4)}
}

var cfg = tun.TunConfig{Name: "EchoFog", Socks: "127.0.0.1:1080", BypassUDPPort: 4433, Resolver: netip.MustParseAddr("198.18.0.2")}

func TestUpStartsTheHelperAndNamesTheAdapter(t *testing.T) {
	ctl, fw := &fakeCtl{}, &fakeLUID{}
	tn := newTunnel(ctl, fw, 42, true)
	if err := tn.Up(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if ctl.started.socks != "127.0.0.1:1080" || ctl.started.udp != 4433 {
		t.Fatalf("started with %+v", ctl.started)
	}
	if ctl.started.hold != "" {
		t.Error("the hold file of v1 is on: the kill switch is the firewall's now, two of them would fight")
	}
	if len(fw.calls) != 1 || fw.calls[0] != 42 {
		t.Fatalf("the firewall was told the adapter %v", fw.calls)
	}
	if !tn.Alive(context.Background()) {
		t.Fatal("not alive after Up")
	}
}

func TestUpFailsWithoutAnAdapter(t *testing.T) {
	ctl, fw := &fakeCtl{}, &fakeLUID{}
	tn := newTunnel(ctl, fw, 0, false)
	if err := tn.Up(context.Background(), cfg); err == nil {
		t.Fatal("Up succeeded although the adapter cannot be found")
	}
	if ctl.stops == 0 {
		t.Error("the helper was left running after a failed Up")
	}
	ctl2 := &fakeCtl{failStart: errors.New("declined")}
	if err := newTunnel(ctl2, fw, 1, true).Up(context.Background(), cfg); err == nil {
		t.Fatal("start error lost")
	}
}

func TestUpRefusesANonLoopbackProxy(t *testing.T) {
	ctl := &fakeCtl{}
	bad := cfg
	bad.Socks = "10.0.0.1:1080"
	if err := newTunnel(ctl, &fakeLUID{}, 1, true).Up(context.Background(), bad); err == nil {
		t.Fatal("accepted a proxy that is not on the loopback")
	}
	if ctl.running {
		t.Fatal("started")
	}
}

func TestDownForgetsTheAdapter(t *testing.T) {
	ctl, fw := &fakeCtl{}, &fakeLUID{}
	tn := newTunnel(ctl, fw, 42, true)
	if err := tn.Up(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := tn.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ctl.running || tn.Alive(context.Background()) {
		t.Fatal("still running")
	}
	if got := fw.calls[len(fw.calls)-1]; got != 0 {
		t.Fatalf("the firewall still names the adapter %d", got)
	}
}

func TestHelperExitIsAnEvent(t *testing.T) {
	ctl, fw := &fakeCtl{}, &fakeLUID{}
	tn := newTunnel(ctl, fw, 42, true)
	if err := tn.Up(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	ctl.running = false
	ctl.onExit(errors.New("the helper exited"))
	select {
	case ev := <-tn.Events():
		if ev.Err == nil {
			t.Fatal("event without a reason")
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	if tn.Alive(context.Background()) {
		t.Fatal("alive after the helper exited")
	}
}

// A stop on purpose is not a loss.
func TestStopOnPurposeIsNotAnEvent(t *testing.T) {
	ctl, fw := &fakeCtl{}, &fakeLUID{}
	tn := newTunnel(ctl, fw, 42, true)
	_ = tn.Up(context.Background(), cfg)
	_ = tn.Down(context.Background())
	select {
	case ev := <-tn.Events():
		t.Fatalf("event %v after Down", ev.Err)
	case <-time.After(100 * time.Millisecond):
	}
}
