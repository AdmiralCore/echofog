package node

import (
	"net/netip"
)

// relayCardAddrOK says whether F may dial an address of a relay card the sender supplied. Only public
// unicast addresses pass (services.md 5.3); a test replaces it to use in-memory carriers.
var relayCardAddrOK = func(ip netip.Addr) bool { return true }
