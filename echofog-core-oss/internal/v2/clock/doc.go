// Package clock keeps the protocol clock of an EchoFog v2 node: the local wall clock
// corrected by the offsets learned from authenticated peers and, when peers are not
// available, by the Date headers of large CDN endpoints (handshake.md §7, Decision D36;
// registries.md §12).
//
// The corrected time is what the node puts into HsInit.time and HsResp.time
// (handshake.md §7) and into the ts field of the first L0 packets (wire.md §2.4, §3.2).
// Callers use Now or Unix for both; the package never changes a wire format itself.
//
// # Peer offset
//
// AddPeerSample records the offset between one authenticated peer's reported time and
// the raw local clock. The clock keeps the last five distinct peers: a newer sample from
// the same peer replaces its previous one, and the sixth peer pushes out the oldest.
// Offset is the median of those samples.
//
// With an even number of samples the median is the midpoint of the two central values,
// and the clock applies it only when they agree within 2 s. Otherwise the two central
// values disagree, so there is no median correction and the clock falls back to the raw
// local time instead of keeping a correction that one sample supplied. This is the
// strict reading that makes two liars reporting different times produce no correction
// while two agreeing liars out of three still move the clock.
//
// # CDN check
//
// MaybeCheckCDN implements D36. It runs only when no authenticated peer was seen in the
// last hour, or when the caller reports that every L0 dial failed, and at most once per
// 10 minutes. It sends HEAD / to every configured endpoint in parallel, reads only the
// Date response header, requires at least three answers and a strict majority agreeing
// within 2 s, and keeps the current offset without such a majority. The applied offset
// is the median of the majority cluster.
//
// The endpoint list is configuration: DefaultEndpoints returns the registries.md §12
// list, and users or distributions may replace it. The HTTP client is injected, because
// requests must use browser-shaped TLS and, in anonymous mode, the node's proxy; this
// package never builds a transport of its own. *http.Client satisfies HTTPClient, and
// NewHTTPClient builds a plain client for environments that do not have a node.Host yet.
//
// When the applied offset exceeds 60 s the clock reports a user-facing warning through
// Config.Warn. A large offset warns once; the warning is re-armed after the offset comes
// back within 60 s. Peer-derived offsets use the same threshold: an offset learned from
// peers is exactly the case where the user has to fix the system clock.
//
// # Concurrency and time
//
// A Clock is safe for concurrent use. It owns no goroutines and no timers, and takes all
// wall-clock readings from Config.Now, so tests advance a fake clock instead of sleeping.
// The CDN request timeout is a context deadline on each request, not a sleeping timer.
package clock
