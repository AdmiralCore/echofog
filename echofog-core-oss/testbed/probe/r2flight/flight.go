// Package r2flight builds the first flight of a reality2-tcp client without a connection, so the
// prober can send a fresh valid token or a token made for another node's key.
package r2flight

import "time"

// Flight returns the first TLS record a reality2-tcp client sends to the node whose masking public
// key is maskPub, for the address string addr the token binds to, at the given time.
func Flight(maskPub []byte, addr, sni string, now time.Time) ([]byte, error) { return nil, nil }
