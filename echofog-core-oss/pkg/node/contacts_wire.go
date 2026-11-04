package node

import (
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/services/contacts"
	"github.com/echofog/echofog-core/internal/services/contactswire"
)

// The assembly point of the contacts port in v1 (T7.0). node.go hands the swarm to the service as
// it is, because the swarm satisfies contacts.Wire; this file is where the dependency on the p2p
// layer is named, and it gives the places that build a service without a network a nil-safe wire
// (internal/services/contactswire).
//
// A typed nil swarm passed as contacts.Wire would look like a live wire; contactsWire avoids that.
func contactsWire(sw *swarm.Swarm) contacts.Wire {
	if sw == nil {
		return nil
	}
	return contactswire.New(sw)
}
