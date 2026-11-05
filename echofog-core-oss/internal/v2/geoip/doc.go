// Package geoip answers which country an IP address belongs to, for the suite choice of wire.md
// §1 (D21). It uses only the built-in offline database: a node MUST NOT query an online GeoIP
// service with peer addresses (K7), and platforms.md §1.2 forbids the online lookup of v1.
//
// # Source and license
//
// The data is DB-IP Lite Country, downloaded from https://download.db-ip.com/free/ (the
// 2026-11 file), licensed under Creative Commons Attribution 4.0 International
// (https://creativecommons.org/licenses/by/4.0/). Attribution: "IP Geolocation by DB-IP
// (https://db-ip.com)". The full notice is in THIRD_PARTY_NOTICES.md. Changes: rows with the
// unknown country code "ZZ" are dropped, adjacent ranges of the same country are merged, and the
// remaining ranges are stored in the compact binary format below.
//
// # Generated data
//
// country.bin is produced by go:generate from the CSV:
//
//	go generate ./internal/v2/geoip
//
// The CSV itself is not committed. Download it to data/dbip-country-lite.csv.gz under this
// package (the data/ directory is ignored by git) and run the command above. The generator entry
// point is gen.go (build-ignored), its logic is internal/dbgen, and the binary format and its
// reader are internal/codec. The embedded data is held below 8 MiB, which is checked by
// TestEmbeddedDataSize.
//
// # Semantics
//
// CountryOf returns the ISO 3166-1 alpha-2 code for a public IPv4 or IPv6 address. One exception
// comes from the source: XK (Kosovo) is a user-assigned DB-IP pseudocode, not part of ISO 3166-1.
// It is kept as in the source, and callers must not treat it as an error. Private, loopback,
// link-local, multicast, the RFC 4291 reserved block fe00::/9 and other reserved or
// special-purpose addresses return unknown, even if the upstream file maps them somewhere.
// Unknown is also returned for addresses the database does not cover and for an invalid
// netip.Addr. An IPv4-mapped IPv6 address is resolved as its IPv4 address, and an IPv6 zone
// (for example fe80::1%eth0) is ignored before the reserved check and the lookup.
//
// "Unknown counts as a border" (wire.md §1 point 3), so callers must treat false as "not known to
// be in the same country" and never as "same country".
package geoip
