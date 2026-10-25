package node

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// circuitRetry is how often a circuit to a peer without a direct link is tried again.
const circuitRetry = 5 * time.Second

var errOpeningCircuit = errors.New("not connected; opening a relay circuit")

// callLink carries call frames to a peer: over the direct link when there is one, else over a
// relay circuit. Somebody behind a symmetric NAT can be reached by no hole punch; before this
// they waited at the door of every call that a person behind another NAT started, while their
// messages, which go through relays, arrived. Frames are signed (signals) or sealed with the room
// key (media), so the relay can neither read nor forge them.
type callLink struct {
	n *Node

	mu      sync.Mutex
	opening map[types.PeerID]time.Time
	viaWarn map[types.PeerID]bool
}

func newCallLink(n *Node) *callLink {
	return &callLink{n: n, opening: map[types.PeerID]time.Time{}, viaWarn: map[types.PeerID]bool{}}
}

func (l *callLink) SendRawFrame(peer types.PeerID, t net_types.MessageType, payload []byte) error {
	if l.n.Swarm.GetPeer(peer) != nil {
		return l.n.Swarm.SendRawFrame(peer, t, payload)
	}
	return l.viaCircuit(peer, t, payload)
}

func (l *callLink) SendUnreliableFrame(peer types.PeerID, t net_types.MessageType, payload []byte) error {
	if l.n.Swarm.GetPeer(peer) != nil {
		return l.n.Swarm.SendUnreliableFrame(peer, t, payload)
	}
	return l.viaCircuit(peer, t, payload)
}

// viaCircuit sends over the circuit to peer, or starts opening one (at most every circuitRetry)
// and says the frame did not go: a media frame must never wait for a relay to answer.
func (l *callLink) viaCircuit(peer types.PeerID, t net_types.MessageType, payload []byte) error {
	rc := l.n.RelayClient
	if rc == nil {
		return errors.New("this peer is not connected")
	}
	if cid, ok := rc.CircuitTo(peer); ok {
		if err := rc.SendCircuitData(cid, t, payload); err == nil {
			l.mu.Lock()
			first := !l.viaWarn[peer]
			l.viaWarn[peer] = true
			if len(l.viaWarn) > 1024 {
				l.viaWarn = map[types.PeerID]bool{peer: true}
			}
			l.mu.Unlock()
			if first {
				l.n.Logger.Info("call: reaching a peer through a relay (no direct link)", "peer", peer.String()[:8])
			}
			return nil
		}
	}
	l.mu.Lock()
	last, busy := l.opening[peer]
	due := !busy || time.Since(last) > circuitRetry
	if due {
		l.opening[peer] = time.Now()
		if len(l.opening) > 1024 {
			l.opening = map[types.PeerID]time.Time{peer: time.Now()}
		}
	}
	l.mu.Unlock()
	if due {
		go func() {
			ctx, cancel := context.WithTimeout(l.n.Ctx, 10*time.Second)
			defer cancel()
			if _, err := rc.OpenCircuitTo(ctx, peer); err != nil {
				l.n.Logger.Info("call: no relay circuit to a peer yet", "peer", peer.String()[:8], "err", err)
			}
		}()
	}
	return errOpeningCircuit
}
