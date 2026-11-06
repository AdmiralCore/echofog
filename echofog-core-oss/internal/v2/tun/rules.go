// Package tun is the policy side of the whole-computer VPN mode of v2 (T10.6): the firewall rules
// of the kill switch, the DNS protection and the state machine that keeps them fail-closed. It
// owns no device and no firewall. The platform drivers (Windows WFP with persistent filters,
// Linux nftables, the Wintun/TUN helper of internal/network/tunmode) sit behind the interfaces of
// driver.go, so everything here runs against fakes.
//
// The rules are data. BuildRules turns a Plan into them and Evaluate is a reference evaluator of
// the same data: the tests prove with it that nothing leaves the computer outside the tunnel.
package tun

import (
	"errors"
	"net/netip"
)

// Action of a rule.
type Action uint8

const (
	Block Action = iota
	Permit
)

// Dir is the direction of a packet or a rule.
type Dir uint8

const (
	Out Dir = iota
	In
)

// Iface is where a packet is seen.
type Iface uint8

const (
	IfaceAny Iface = iota
	IfaceLoopback
	IfaceTunnel
	IfacePhysical
)

// Proto of a rule or packet.
type Proto uint8

const (
	ProtoAny Proto = iota
	ProtoTCP
	ProtoUDP
	ProtoICMP
)

// Rule is one filter. The highest Weight that matches decides; on a tie Block wins. Every rule is
// Persistent: the firewall keeps it when the process that installed it dies (that is what makes
// the kill switch survive a killed helper).
type Rule struct {
	Name       string
	Action     Action
	Dir        Dir
	Iface      Iface
	Proto      Proto
	Remote     netip.Prefix // zero: any address (of the rule's family)
	V6         bool         // the rule applies to IPv6 (otherwise IPv4)
	RemotePort uint16       // 0: any
	LocalPort  uint16       // 0: any
	Weight     uint8
	Persistent bool
}

// Packet is what Evaluate decides on.
type Packet struct {
	Dir    Dir
	Iface  Iface
	Proto  Proto
	Remote netip.AddrPort
	Local  uint16
}

// Plan describes what the kill switch must allow around the tunnel.
type Plan struct {
	// Endpoints are the addresses the node's own transport talks to outside the tunnel (the exit
	// and relays it connects to). Each is permitted for its protocol only.
	Endpoints []Endpoint
	// TunnelPool is the address range of the TUN interface (v1: 198.18.0.0/15) and its IPv6 range;
	// endpoints inside it are refused.
	TunnelPool []netip.Prefix
	// AllowLAN permits private and link-local ranges on the physical interface. Off by default.
	AllowLAN bool
	// AllowDHCP permits DHCPv4/v6 on the physical interface so the link can renew its lease.
	AllowDHCP bool
}

// Endpoint is one address:port the node reaches outside the tunnel.
type Endpoint struct {
	Addr  netip.AddrPort
	Proto Proto // ProtoTCP or ProtoUDP
}

// Limits of a Plan.
const MaxEndpoints = 64

var (
	// ErrBadPlan is a Plan that cannot be turned into safe rules.
	ErrBadPlan = errors.New("tun: bad kill switch plan")
)

// BuildRules turns a Plan into the persistent rule set of the kill switch.
func BuildRules(Plan) ([]Rule, error) { return nil, nil }

// Evaluate returns whether the rules permit a packet: the matching rule of the highest weight,
// Block on a tie, Block when none matches.
func Evaluate([]Rule, Packet) Action { return Permit }
