// Package clock keeps the protocol clock of an EchoFog v2 node: the local wall clock
// corrected by the offsets learned from authenticated peers and, when peers are not
// available, by the Date headers of large CDN endpoints (handshake.md §7, Decision D36;
// registries.md §12).
//
// The corrected time is what the node puts into the ts field of the first L0 packets
// (wire.md §2.4, §3.2). The handshake (HsInit.time, HsResp.time and its +-600 s check) and card
// lifetimes use the raw clock, never the offset (handshake.md §7). The package never changes a
// wire format itself.
//
// # Peer offset
//
// AddOutgoingSample records the time one peer of a session this node dialed reported. Incoming
// sessions give no sample. The clock keeps one sample per /16 network (IPv6: /32), the last five
// networks; a newer sample from a network replaces its previous one. The offset is the median of
// the samples and is used only when at least 3 networks gave samples. One update moves it by at most
// 120 s and it never exceeds 600 s from the raw clock; a larger correction needs the CDN check.
//
// With an even number of samples the median is the midpoint of the two central values, applied only
// when they agree within 2 s; otherwise there is no estimate and the offset drifts back to the raw
// clock by the same step limit.
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
// list, and users or distributions may replace it. The HTTP client is injected through
// Config.HTTP. NewHTTPClient and StartupConfig build the production client: uTLS with the
// Chrome ClientHello and, in anonymous mode, the node's SOCKS5 proxy (T11.6a). The
// products (efv2, the desktop facade, mobile, the server node) pass that client at start,
// so the CDN check is not left off with ErrNoHTTPClient (D36, PA-FIX-4).
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
