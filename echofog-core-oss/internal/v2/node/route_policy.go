package node

import "github.com/echofog/echofog-core/internal/v2/routepolicy"

// RouteReport is the rule applied to the last dial of a peer, for the connection report (T11.2).
type RouteReport struct {
	RuleID       string
	TableVersion string
	Net          routepolicy.NetClass
	Dest         routepolicy.DestClass
	// ViaEntry: the first hop was an entry node inside the own country.
	ViaEntry bool
}

// SetNetClass changes the kind of the client's network (T6.4).
func (r *Runtime) SetNetClass(c routepolicy.NetClass) {}

// NetClass returns the current kind of the client's network.
func (r *Runtime) NetClass() routepolicy.NetClass { return routepolicy.NetUnknown }

// RouteReport returns the rule applied to the last dial of peer.
func (r *Runtime) RouteReport(peer DeviceID) (RouteReport, bool) { return RouteReport{}, false }
