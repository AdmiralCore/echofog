// Package node is the v2 runtime (02-architecture §3.1, AD5): it owns the local identity, the
// carrier listeners, the session table and the service registry, and it hands services a Host
// whose handler receives a Peer.
//
// A node is assembled once by the application (desktop, mobile, server): it gets an identity, a
// card builder, a store and a set of carrier listeners, then Start accepts sessions on those
// listeners and lets modules open streams and register services. The runtime does not implement
// any service itself; node/modules.go has one registration file per service so that service
// branches do not conflict when they merge.
//
// D37 (wire.md §2.1): a node without a camouflaged listener has no public card. The runtime does
// not refuse to start for that reason: it publishes nothing public, tells the operator through
// Events, and keeps serving peers and invitees with the peer card.
package node
