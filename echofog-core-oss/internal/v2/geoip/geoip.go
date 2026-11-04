// Package geoip answers one question for wire.md §1 (D21): which country an IP address belongs
// to, using only the built-in offline database (K7).
package geoip

import "net/netip"

// dbBytes holds the embedded compact database. The generator replaces the placeholder.
var dbBytes []byte

// CountryOf reports the ISO 3166-1 alpha-2 country of a public IPv4 or IPv6 address.
func CountryOf(a netip.Addr) (string, bool) {
	return "", false
}
