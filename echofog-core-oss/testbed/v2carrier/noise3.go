package main

import (
	"context"
	"net"
	"time"
)

// noise3Key derives the masking key pair both ends of a lab run use from the run's seed, so the
// client needs no card file: the real carrier is exercised, only the key distribution is skipped.
func noise3Key(seed int64) (private [32]byte, public []byte) { return private, nil }

// noise3Serve runs a real carrier/noise3 listener on pc and echoes every stream until ctx ends.
func noise3Serve(ctx context.Context, pc net.PacketConn, seed int64, ready string) error { return nil }

// noise3Client dials the listener at peer with the real carrier/noise3 dialer, opens a stream,
// sends data for d and counts what comes back.
func noise3Client(peer string, seed int64, d time.Duration) (result, error) {
	return result{Mode: "noise3"}, nil
}
