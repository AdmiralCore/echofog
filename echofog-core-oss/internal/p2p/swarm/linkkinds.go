package swarm

import "github.com/echofog/echofog-core/internal/network"

// LinkKinds counts the node's live links by what carries them, for a connection report: "quic"
// (QUIC with the node's noise obfuscation), "quic-reality" (QUIC disguised as a visit to a cover
// site), "tcp" (browser-TLS mimicry over TCP) and "ws" (WebSocket, behind a CDN). It names no peer.
func (s *Swarm) LinkKinds() map[string]int {
	out := map[string]int{}
	for _, p := range s.GetAllPeers() {
		p.mu.RLock()
		tr, reality := p.transport, p.primaryReality
		p.mu.RUnlock()
		if tr == nil {
			continue
		}
		kind := network.LinkKind(tr.GetConn())
		if kind == network.ProtocolQUIC && reality {
			kind = "quic-reality"
		}
		out[kind]++
	}
	return out
}

// PrefersTCPHere reports whether the transport learned that QUIC does not get through on this
// network (UDP blocked or throttled), so TCP goes first here.
func (s *Swarm) PrefersTCPHere() bool {
	if f, ok := s.netTransport.(interface{ PrefersTCPHere() bool }); ok {
		return f.PrefersTCPHere()
	}
	return false
}
