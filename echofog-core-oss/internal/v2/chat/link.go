package chat

import (
	"context"
	"errors"
	"time"
)

// StreamLink is the Link over chat/2 streams: one DELIVER per stream, answered by DELIVERED.
type StreamLink struct {
	// Open opens a chat/2 stream to a device (node.Host.Open with service.Chat).
	Open func(ctx context.Context, device [32]byte) (Stream, error)
	// After is the timer (AD13); nil means time.After.
	After func(d time.Duration) <-chan time.Time
}

// Deliver sends one envelope and waits for DELIVERED for DeliverTimeout.
func (l *StreamLink) Deliver(ctx context.Context, device [32]byte, envelope []byte) (uint32, error) {
	return 0, errors.New("not implemented")
}
