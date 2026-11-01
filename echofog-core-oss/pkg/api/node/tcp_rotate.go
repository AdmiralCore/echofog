package node

import (
	"strings"

	"github.com/echofog/echofog-core/internal/config"
)

// freezeCountries are the countries whose networks run the TSPU freeze: a TCP connection to a
// foreign hosting address works for about 16 KB of the answer and then goes silent. tcp_rotate
// "auto" rotates the TCP links that cross the border from one of them.
var freezeCountries = map[string]bool{"RU": true}

// applyTCPRotateAuto installs the country rule for tcp_rotate "auto" (the node layer has already
// handled "on" and "off"). A peer on the local network or in the node's own country is not behind
// the border; a country that cannot be placed is not assumed to run the freeze, since rotation
// costs time and the transport learns from a frozen link anyway.
func (n *NodeApi) applyTCPRotateAuto() {
	c := n.node.Cfg
	if c == nil || n.node.Net == nil {
		return
	}
	if m := c.AntiDPI.TCPRotate; (m != "" && m != config.TCPRotateAuto) || c.AntiDPI.MicroChunkTCP {
		return
	}
	if n.node.Swarm != nil {
		// behind the freeze a guard of the own country is preferred: its link never crosses the border
		n.node.Swarm.SetDomesticFunc(func(addr string) bool {
			return freezeCountries[strings.ToUpper(n.homeCountry())] && n.peerInHomeRegion(addr)
		})
	}
	n.node.Net.SetTCPRotate(func(addr string) bool {
		if !freezeCountries[strings.ToUpper(n.homeCountry())] {
			return false
		}
		return !n.peerInHomeRegion(addr)
	})
}
