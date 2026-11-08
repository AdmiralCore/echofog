package scenarios

import (
	"context"
	"net/netip"

	"github.com/echofog/echofog-core/testbed/gate"
)

func natScenario() Scenario {
	return Scenario{Name: "v2-nat", Title: "NAT matrix: punch where it can, the relay where it cannot", Live: liveNAT, Run: runNat}
}

// runNat is not written yet: nothing measured, so nothing green.
func runNat(_ context.Context, env Env) Result {
	return Result{Name: "v2-nat", Mode: "model", Seed: env.Seed, Rows: []gate.Row{{
		ID: "v2-nat/implemented", Metric: "scenario", Value: "not written", Limit: "measured", Status: gate.NotMeasured, Source: "T15.1"}}}
}

// NATType is the behaviour of a NAT towards UDP.
type NATType int

const (
	FullCone NATType = iota
	AddressRestricted
	PortRestricted
	Symmetric
)

func (t NATType) String() string { return "?" }

type natBox struct{}

func newNATBox(NATType, netip.Addr, uint16) *natBox                { return &natBox{} }
func (n *natBox) Send(netip.AddrPort) netip.AddrPort               { return netip.AddrPort{} }
func (n *natBox) Accepts(from netip.AddrPort, extPort uint16) bool { return false }
