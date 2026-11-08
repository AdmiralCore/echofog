package node

import (
	"errors"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
)

// v2Hooks replace the network edges of the v2 stack. Production leaves them nil; tests put two
// stacks of one process on in-memory carriers.
type v2Hooks struct {
	// Listen returns the carrier listeners the runtime accepts on and the listeners its cards
	// advertise. Nil: the node's own settings decide.
	Listen func(dev *identity.Device) ([]carrier.Listener, []cards.Listener)
	// Dialer wraps the runtime for the link service. Nil: the runtime dials.
	Dialer func(rt *node.Runtime) links.Dialer
}

// v2Stack is the running protocol 2 runtime of this facade.
type v2Stack struct{}

var errV2NotRun = errors.New("v2 runtime is not built")

// startV2 builds and starts the v2 runtime and attaches it to the facade (protocol 2 only).
func (n *NodeApi) startV2() error { return errV2NotRun }

// stopV2 stops the v2 runtime and releases everything it holds.
func (n *NodeApi) stopV2() {}

// v2App is the application layer of the running v2 runtime, nil when none runs.
func (n *NodeApi) v2App() *app.App { return nil }

// V2Running reports whether a v2 runtime is up.
func (n *NodeApi) V2Running() bool { return false }

// v2Ingested counts the peer cards the app layer took from a knock answer (tests).
func (n *NodeApi) v2Ingested() int { return 0 }
