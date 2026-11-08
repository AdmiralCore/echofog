package scenarios

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/testbed/behaviour"
)

// World is a set of real v2 nodes in one process.
type World struct{ dir string }

// NodeSpec describes a node.
type NodeSpec struct {
	Name       string
	Server     bool                   // the server profile: DHT server, mailbox and relay roles, role server in its card
	Listen     bool                   // a real noise3 listener on loopback
	Hidden     bool                   // a card that is not published
	Chat       bool                   // chat/2 on
	Netem      *behaviour.NetemConfig // the listener sits behind an in-process netem
	CardMutate func(*cards.Config)    // changes the card material before the builder seals it
	Mutate     func(*node.Config)     // changes the runtime configuration
}

// Node is one real node.
type Node struct {
	Name  string
	ID    node.Identity
	RT    *node.Runtime
	Netem *behaviour.Netem
}

// NewWorld returns an empty world; dir holds the nodes' stores.
func NewWorld(dir string) *World { return &World{dir: dir} }

// Add builds a node.
func (w *World) Add(NodeSpec) (*Node, error) { return nil, errors.New("not implemented") }

// StartAll starts every node that is not started.
func (w *World) StartAll(context.Context) error { return errors.New("not implemented") }

// Close stops every node and netem; it may be called twice.
func (w *World) Close() {}

// Dial opens a session from n to to, with to's peer card.
func (n *Node) Dial(context.Context, *Node) (node.Peer, *mux.Session, error) {
	return node.Peer{}, nil, errors.New("not implemented")
}

// SuiteOf names the carrier suite of n's session with to.
func (n *Node) SuiteOf(*Node) string { return "" }

// PeerCard is the node's own peer card.
func (n *Node) PeerCard() (*objects.Card, error) { return nil, errors.New("not implemented") }
