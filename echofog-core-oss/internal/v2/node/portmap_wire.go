package node

import (
	"net"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/portmap"
)

// mappedPort is a listener port the node asked the home router to forward.
type mappedPort struct {
	proto portmap.Proto
	port  uint16
}

// startPortMapping asks the router to forward the ports of the node's public listeners (T6.5b).
// It does nothing unless the profile enables port_mapping and the node is visible: a hidden node
// publishes no address, so it opens nothing to the internet. Contact and invitation listeners are
// never passed in. The router's answer is not published until peers confirm the public IP
// (portMapChanged).
func (r *Runtime) startPortMapping(public []carrier.Listener) {
	if !r.cfg.Profile.PortMapping || r.vis != cards.Visible {
		return
	}
	var ports []mappedPort
	for _, l := range public {
		var p mappedPort
		switch a := l.Addr().(type) {
		case *net.UDPAddr:
			p = mappedPort{portmap.UDP, uint16(a.Port)}
		case *net.TCPAddr:
			p = mappedPort{portmap.TCP, uint16(a.Port)}
		default:
			continue
		}
		if p.port == 0 {
			continue
		}
		dup := false
		for _, q := range ports {
			dup = dup || q == p
		}
		if dup {
			continue
		}
		if err := r.pm.Add(p.proto, p.port); err != nil {
			r.log.Warn("node: port mapping", "proto", string(p.proto), "err", err)
			continue
		}
		ports = append(ports, p)
	}
	r.pmMu.Lock()
	r.pmPorts = ports
	r.pmMu.Unlock()
}

// portMapChanged makes the cards follow the router: a mapped port is announced when peers have
// confirmed the router's external IP, and the configured port otherwise. It runs on the port
// mapping goroutine and on Confirm callers; pmApply keeps a stale view from overwriting a newer one.
func (r *Runtime) portMapChanged() {
	r.pmMu.Lock()
	ports := append([]mappedPort(nil), r.pmPorts...)
	r.pmMu.Unlock()
	if len(ports) == 0 {
		return
	}
	r.pmApply.Lock()
	defer r.pmApply.Unlock()
	cands := r.pm.Candidates()
	changed := 0
	for _, p := range ports {
		var ext uint32
		for _, c := range cands {
			if c.Proto == p.proto && c.Internal == p.port {
				ext = uint32(c.External.Port())
			}
		}
		n, err := r.cfg.Cards.SetPublicPort(p.proto == portmap.UDP, uint32(p.port), ext)
		if err != nil {
			r.log.Warn("node: cards did not follow the mapped port", "err", err)
			continue
		}
		changed += n
	}
	if changed > 0 {
		r.events.Emit(Event{Kind: EventPortMapping, Message: "forwarded ports changed", At: r.clock.Now()})
	}
}
