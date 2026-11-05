// Package session establishes an EchoFog v2 session (docs/protocol/v2/handshake.md §3–§9,
// wire.md §6.2, §8.5, §10) on one carrier and hands it to the mux, keeps the session table, and
// owns the session-level state that must outlive the process.
//
// Dial runs the initiator side on stream 0: HS_HELLO and message 1 in one flight, the XKhfs or
// XXhfs handshake, then mux.NewSession. Accept runs the responder side of one carrier accepted
// from a listener, or hands stream 0 to the mux.Binder when it starts with PATH_BIND (a new path
// of an existing session, wire.md §8.2). Every other first frame closes the carrier silently.
//
// Table keeps one live session per (local identity, peer device ID) (02-architecture AD5):
// concurrent dials share one flight, and a simultaneous dial by both sides is resolved by keeping
// the session whose initiator has the smaller device ID, the other leaving with GOAWAY{no-error}
// (wire.md §7.3). Table also closes idle sessions and enforces the platform limit (platforms.md
// §2: 32 sessions when mem_cap_mb is set).
//
// Established.GoAway sends GOAWAY and waits for the drain; the mux ends the session when the
// streams of both sides have ended or after 15 s. Control receives CARD_UPDATE into a CardBook
// (objects.md §3.6) and PushCard sends a new card of this identity to peers that negotiated
// card-push.
//
// State that the specification keeps between restarts (AD13) lives in the store: KeyBindings
// (handshake key ↔ device ID, handshake.md §7, S15) with an AbuseLog counted by source, and the
// CardBook itself. The caller opens them over separate namespaces:
//
//	store.Store(session.BindingsNamespace) → NewKeyBindings
//	store.Store(session.CardsNamespace)    → OpenCardBook
//	store.Store(session.AbuseNamespace)    → NewAbuseLog
//
// The package does not run identity policy or own identities: the caller passes a LocalIdentity
// (the device keys and the card for message 3), a carrier.Clock and, for a responder, the
// responder configuration. It never logs keys and never destroys the caller's identity.
//
// Sealed mode (wire.md §8.5) is a property of the carrier: Dial and Accept hand the stream to
// mux.NewSession, which makes the single StartSealing call after the last handshake frame. The
// handshake reader never reads ahead, so the switch point is exact.
package session
