// Package congestion picks how fast a QUIC connection sends. Stock quic-go only has Cubic,
// which reads every lost packet as congestion: on the lossy links of filtered networks (1% loss
// is ordinary on a long international path) it falls to a few Mbit/s. BBR estimates the
// bottleneck rate and round trip instead, and Brutal sends at a fixed rate whatever is lost.
//
// bbr, brutal and common come from hysteria (MIT, see LICENSE-hysteria.md), and plug into the
// apernet/quic-go fork that hysteria ships with, which lets a connection change its controller.
package congestion

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/apernet/quic-go"
	qc "github.com/apernet/quic-go/congestion"
	"github.com/echofog/echofog-core/internal/network/congestion/bbr"
	"github.com/echofog/echofog-core/internal/network/congestion/brutal"
)

type Mode string

const (
	// BBR is the default: as fast as the path allows, fair to other traffic, robust to loss.
	BBR Mode = "bbr"
	// Cubic is quic-go's own controller, as before.
	Cubic Mode = "cubic"
	// Brutal sends at BrutalBps whatever the path loses, raising the rate to make up for it. It
	// is only for a user who knows their line rate: set too high it floods the path and is
	// unfair to everything else on it.
	Brutal Mode = "brutal"
)

// Config is a connection's congestion control.
type Config struct {
	Mode       Mode
	BrutalBps  uint64 // bytes per second this side sends at in Brutal mode
	BBRProfile string // "conservative", "standard" (default) or "aggressive"
}

// EnvOverride is read by FromEnv: "bbr", "cubic" or "brutal:<Mbit/s>", for experiments.
const EnvOverride = "ECHOFOG_CONGESTION"

// Parse reads a mode as configured: "" and "bbr" are BBR, "cubic", or "brutal" with mbps > 0.
func Parse(mode string, brutalMbps int) (Config, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(mode))) {
	case "", BBR:
		return Config{Mode: BBR}, nil
	case Cubic, "reno":
		return Config{Mode: Cubic}, nil
	case Brutal:
		if brutalMbps <= 0 {
			return Config{}, fmt.Errorf("congestion: brutal needs a rate in Mbit/s")
		}
		return Config{Mode: Brutal, BrutalBps: uint64(brutalMbps) * 1_000_000 / 8}, nil
	}
	return Config{}, fmt.Errorf("congestion: unknown mode %q (bbr, cubic or brutal)", mode)
}

// FromEnv returns c, or what ECHOFOG_CONGESTION says instead when it is set and valid.
func FromEnv(c Config) Config {
	v := os.Getenv(EnvOverride)
	if v == "" {
		return c
	}
	mode, rate, _ := strings.Cut(v, ":")
	mbps, _ := strconv.Atoi(rate)
	if parsed, err := Parse(mode, mbps); err == nil {
		return parsed
	}
	return c
}

// Apply installs c on conn. Cubic leaves quic-go's own controller in place.
func Apply(conn *quic.Conn, c Config) {
	switch c.Mode {
	case Cubic:
	case Brutal:
		conn.SetCongestionControl(brutal.NewBrutalSender(c.BrutalBps, false))
	default:
		profile, err := bbr.ParseProfile(c.BBRProfile)
		if err != nil {
			profile = bbr.ProfileStandard
		}
		conn.SetCongestionControl(bbr.NewBbrSender(bbr.DefaultClock{},
			seedPacketSize(conn.InitialPacketSize(), bbr.GetInitialPacketSize(conn.RemoteAddr())), profile))
	}
}

// seedPacketSize never seeds the controller above the size QUIC itself starts at: a path MTU
// probe landing between the two would look like a shrink to the controller (as in hysteria).
func seedPacketSize(quicSize, byAddr qc.ByteCount) qc.ByteCount {
	if quicSize <= 0 {
		return byAddr
	}
	return min(quicSize, byAddr)
}
