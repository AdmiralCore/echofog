package routing

import "github.com/echofog/echofog-core/pkg/api/types"

func (rc *RelayClient) MeasuredFlags(id types.PeerID) (priceKnown bool, pingKnown bool) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	_, priceKnown = rc.relayPrices[id]
	_, pingKnown = rc.relayPings[id]
	return
}
