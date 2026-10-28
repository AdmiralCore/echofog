package node

import (
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/config"
)

// guard_mode "auto" by country. Guards cost a user in a free country its direct links (calls and
// streams go through a relay) and protect nothing there, so "auto" stands down where the node's
// own country does not censor. It holds guards in a censoring country, and while the country is
// not known yet: unknown counts as censored. Once a censoring country is seen the decision sticks
// until the node restarts, so a lookup that later fails or a misplaced address cannot drop the
// guards in the middle of a session. The country is that of the node's own public address (or
// anti_dpi.home_country): a node behind a foreign VPN is placed abroad, which is right, since the
// censor then only sees the VPN.

// censoringCountries are the ISO 3166-1 alpha-2 codes of the countries whose networks run DPI
// against VPNs or foreign traffic, where guard mode stays on.
var censoringCountries = map[string]bool{
	"RU": true, "BY": true, "IR": true, "CN": true, "TM": true, "KP": true, "MM": true,
	"UZ": true, "TJ": true, "KZ": true, "AZ": true, "TR": true, "SY": true, "IQ": true,
	"SA": true, "AE": true, "QA": true, "OM": true, "BH": true, "EG": true, "ET": true,
	"ER": true, "PK": true, "VN": true, "LA": true, "CU": true, "VE": true, "NI": true,
}

// censoringCountry reports whether guard mode stays on in the country cc ("" is not known: on).
func censoringCountry(cc string) bool {
	cc = strings.ToUpper(strings.TrimSpace(cc))
	return cc == "" || censoringCountries[cc]
}

const (
	guardAutoFirst = 3 * time.Second  // the first look, after the NAT check had a moment
	guardAutoEvery = 30 * time.Second // then: the address may change (another network)
)

// guardAutoDeciding: the country matters, the node being a user's one with guard_mode "auto" and
// obfuscation on.
func (n *NodeApi) guardAutoDeciding() bool {
	c := n.node.Cfg
	return c != nil && !c.IsServer() && c.AntiDPI.Obfs == config.ObfsNode &&
		(c.AntiDPI.GuardMode == "" || c.AntiDPI.GuardMode == config.GuardAuto)
}

// runGuardAuto keeps the swarm told whether guard_mode "auto" may stand down, until the node stops.
func (n *NodeApi) runGuardAuto() {
	ctx := n.node.Ctx
	if ctx == nil || n.node.Swarm == nil {
		return
	}
	n.node.Swarm.SetGuardsFree(false)
	sticky := false
	t := time.NewTimer(guardAutoFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		next := guardAutoEvery
		if n.guardAutoDeciding() { // no lookup of the own address where "auto" decides nothing
			cc := n.homeCountry()
			if cc == "" {
				next = guardAutoFirst // the lookup was only started: look again soon
			} else if censoringCountry(cc) {
				sticky = true
			}
			n.node.Swarm.SetGuardsFree(!sticky && !censoringCountry(cc))
		}
		t.Reset(next)
	}
}
