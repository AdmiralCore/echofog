// Package r2flight builds the first flight of a reality2-tcp client without a connection, so the
// prober can send a fresh valid token or a token made for another node's key.
package r2flight

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
)

// Flight returns the first TLS record a reality2-tcp client sends to the node whose masking public
// key is maskPub, for the address string addr the token binds to, at the given time. Every call
// uses a new ephemeral key, so two flights are never the same bytes (the node would take the second
// for a replay).
func Flight(maskPub []byte, addr, sni string, now time.Time) ([]byte, error) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	uc := utls.UClient(client, &utls.Config{ServerName: sni, InsecureSkipVerify: true}, utls.HelloChrome_Auto) // #nosec G402 -- the hello is never sent to a peer that could verify anything
	if err := reality2.PrepareTCPHello(uc, maskPub, addr, func() time.Time { return now }); err != nil {
		return nil, fmt.Errorf("r2flight: %w", err)
	}
	go func() { _ = uc.Handshake() }() // writes the ClientHello into the pipe, then waits for a server that never comes

	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [5]byte
	if _, err := io.ReadFull(server, hdr[:]); err != nil {
		return nil, fmt.Errorf("r2flight: read the record header: %w", err)
	}
	if hdr[0] != 0x16 {
		return nil, errors.New("r2flight: the first record is not a handshake record")
	}
	body := make([]byte, int(hdr[3])<<8|int(hdr[4]))
	if _, err := io.ReadFull(server, body); err != nil {
		return nil, fmt.Errorf("r2flight: read the record: %w", err)
	}
	return append(hdr[:], body...), nil
}
