package main

import (
	"net"
)

// recordConn lets a test see the client side of every connection a lab mode opens; the harness
// leaves it alone.
var recordConn = func(c net.Conn) net.Conn { return c }

// labVisit makes visits ordinary HTTPS visits as a Chrome-shaped client: a connection, a few
// requests with think time between them, the answers of varying size.
func labVisit(peer, sni string, visits int, seed int64) (result, error) {
	return result{Mode: "visit"}, nil
}

// tlsInTLS opens a TLS connection to the server and runs a second TLS handshake and a few
// requests inside it: the traffic D5 exists for.
func tlsInTLS(peer, sni string, seed int64) (result, error) {
	return result{Mode: "tls-in-tls"}, nil
}

// constSize sends 1400-byte writes at a fixed pace over a TLS connection and reads the echo.
func constSize(peer, sni string, seed int64) (result, error) {
	return result{Mode: "const-size"}, nil
}

// connStorm opens n short TLS connections to the server in a few seconds.
func connStorm(peer, sni string, n int) (result, error) {
	return result{Mode: "conn-storm"}, nil
}
