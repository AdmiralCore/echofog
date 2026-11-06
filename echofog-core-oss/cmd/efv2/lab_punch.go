package main

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/punch"
)

// labPunch is the lab-only hole punching setup, switched on by EFV2_PUNCH=1 (the test bed sets it):
//
//	EFV2_PUBLIC_IP  the external IP of the node's router (the lab has no three networks to vote it)
//	EFV2_NAT        cone, or symmetric:LO-HI for the NAT the router imitates
//
// A real node learns both from OBSERVED_ADDR; the lab cannot, because every public node of the test
// bed sits in one /16 (session.Observer counts a /16 as one voice).
type labPunch struct {
	punch    *node.Noise3Punch
	features []uint32
	natInfo  punch.NATInfo
	hasNAT   bool
}

func labFromEnv() (labPunch, error) {
	var l labPunch
	if os.Getenv("EFV2_PUNCH") != "1" {
		return l, nil
	}
	ip, err := netip.ParseAddr(os.Getenv("EFV2_PUBLIC_IP"))
	if err != nil {
		return l, fmt.Errorf("EFV2_PUBLIC_IP: %w", err)
	}
	l.punch = &node.Noise3Punch{PublicIP: func() (netip.Addr, bool) { return ip, true }}
	l.features = []uint32{uint32(frame.FeaturePathMigration), uint32(frame.FeatureObservedAddr)}
	spec := strings.TrimSpace(os.Getenv("EFV2_NAT"))
	switch {
	case spec == "" || spec == "cone":
		l.natInfo, l.hasNAT = punch.NATInfo{Class: punch.NATCone}, true
	case strings.HasPrefix(spec, "symmetric:"):
		lo, hi, ok := strings.Cut(strings.TrimPrefix(spec, "symmetric:"), "-")
		a, e1 := strconv.ParseUint(lo, 10, 16)
		b, e2 := strconv.ParseUint(hi, 10, 16)
		if !ok || e1 != nil || e2 != nil || b < a {
			return l, fmt.Errorf("EFV2_NAT %q: want symmetric:LO-HI", spec)
		}
		// the router imitates a block; the lab states the samples a real node would have collected
		l.natInfo, l.hasNAT = punch.NATInfo{Class: punch.NATSymmetric, Lo: uint16(a), Hi: uint16(b), Samples: 3}, true
	default:
		return l, fmt.Errorf("EFV2_NAT %q: want cone or symmetric:LO-HI", spec)
	}
	return l, nil
}

func (l labPunch) punchNet() node.PunchNetwork {
	if l.punch == nil {
		return nil
	}
	return l.punch
}

func (l labPunch) natFunc() func() punch.NATInfo {
	if !l.hasNAT {
		return nil
	}
	info := l.natInfo
	return func() punch.NATInfo { return info }
}
