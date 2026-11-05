// Package selfcover is the `cover: self` mode of reality2 (T2.6b-2, wire.md §3.4): the node
// serves the operator's own website and, on the same ports, the admitted reality2 connections,
// with one certificate and one TLS configuration.
//
// # Why it matches
//
// A mirrored foreign cover needs a TLS stack that can reproduce another server's cipher choice,
// extension order and record packing, which crypto/tls cannot (T2.6b-1 measured it: 4 of 20 popular
// covers over TCP, none over QUIC). Here there is nothing to reproduce. A stranger's TCP
// connection is given to the website's http.Server in this process, with the bytes already read;
// a stranger's QUIC packets are forwarded by the reality2 gate to an HTTP/3 server on loopback.
// Admitted connections use the very tls.Config (TCP) or the very tls.Config and quic.Config (QUIC)
// of the website, and the same certificate chain from ACME, so the handshake an observer sees is
// the same code with the same inputs: no padding, no profile to keep up to date.
//
// # Three levels of cover
//
//   - Own domain: Domain is the operator's name, ACME validates it (tls-alpn-01 on TCP 443, or
//     http-01 on TCP 80).
//   - DDNS subdomain (DuckDNS and the like): the same, the CA validates whatever name resolves to
//     the node. Nothing in the code depends on who owns the zone.
//   - Only for one's own (no domain): there is no cover; that is noise3 (T2.4), not this package.
//
// # The website
//
// Content is a folder or a proxy target. There is no default site: a thousand nodes serving one
// template page would be a fingerprint of its own. An empty folder starts with a warning. Dot-files
// and directory listings are not served.
//
// # Certificates
//
// Certs runs ACME with golang.org/x/crypto/acme: an ECDSA P-256 key per certificate, the account
// key and the certificate kept in the carrier.KV store, renewal 30 days before expiry (a third of
// the lifetime for short-lived certificates) with a retry after about an hour. The ACME terms of
// service must be accepted explicitly (ACMEConfig.AcceptTOS). The tests run against an in-process
// ACME server; nothing here contacts a real CA unless DirectoryURL is left empty.
//
// # Re-measurement
//
// At start, every uniform(5, 7) hours and whenever the certificate changes, Node.Remeasure
// measures the website and a selected connection over every enabled transport with the profile code
// of T2.6b-1, stores the website's profile and compares the two (CompareOwn). The records after
// the handshake are left out: on the website they are application data, on a selected connection
// they are the carrier's first frames, whose shape is the padding profile's business (wire.md
// §4.3). A report that is not OK means the equivalence broke and the suite must not stay enabled.
//
// # TCP
//
// reality2-tcp proper (the carrier, the mux, sealed mode) is T2.8. This package supplies its
// server side up to the TLS connection: the front of wire.md §4.2 (first record within 10 s,
// at most 256 connections before the token, a slot released as soon as the token is checked,
// excess connections handed to the website unread) and AcceptTCP, which returns a tls.Conn of the
// website's configuration.
package selfcover
