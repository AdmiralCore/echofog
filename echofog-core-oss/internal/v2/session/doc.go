// Package session establishes an EchoFog v2 session (docs/protocol/v2/handshake.md §3–§9,
// wire.md §6.2, §8.5, §10) on one carrier and hands it to the mux.
//
// Dial runs the initiator side on stream 0: HS_HELLO and message 1 in one flight, the XKhfs or
// XXhfs handshake, then mux.NewSession. Accept runs the responder side of one carrier accepted
// from a listener, or hands stream 0 to the mux.Binder when it starts with PATH_BIND (a new path
// of an existing session, wire.md §8.2). Every other first frame closes the carrier silently.
//
// The package does not run timers or own identities: the caller passes a LocalIdentity (the
// device keys and the card for message 3), a carrier.Clock and, for a responder, the responder
// configuration. It never stores keys, never logs and never destroys the caller's identity.
//
// Sealed mode (wire.md §8.5) is a property of the carrier: Dial and Accept hand the stream to
// mux.NewSession, which makes the single StartSealing call after the last handshake frame. The
// handshake reader never reads ahead, so the switch point is exact.
package session
