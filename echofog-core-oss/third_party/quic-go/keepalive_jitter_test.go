package quic

import (
	"testing"
	"time"

	"github.com/apernet/quic-go/internal/monotime"
	"github.com/apernet/quic-go/internal/utils"
)

// Each keep-alive of an idle connection comes at its own random offset within ±jitter of the
// period, not on a fixed clock.
func TestKeepAliveJitter(t *testing.T) {
	c := &Conn{
		config:            &Config{KeepAlivePeriod: 10 * time.Second, KeepAliveJitter: 0.3},
		keepAliveInterval: 10 * time.Second,
		rttStats:          utils.NewRTTStats(),
	}
	c.lastPacketReceivedTime = monotime.Now()
	seen := map[time.Duration]bool{}
	for range 200 {
		c.drawKeepAliveJitter()
		d := c.nextKeepAliveTime().Sub(c.lastPacketReceivedTime)
		if d < 7*time.Second || d > 13*time.Second {
			t.Fatalf("keep-alive after %v, outside 7..13 s", d)
		}
		seen[d.Round(100*time.Millisecond)] = true
	}
	if len(seen) < 20 {
		t.Fatalf("only %d distinct keep-alive delays in 200 draws", len(seen))
	}

	c.config.KeepAliveJitter = 0
	c.drawKeepAliveJitter()
	if d := c.nextKeepAliveTime().Sub(c.lastPacketReceivedTime); d != 10*time.Second {
		t.Fatalf("no jitter asked, keep-alive after %v", d)
	}
}
