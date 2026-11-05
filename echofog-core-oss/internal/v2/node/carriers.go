package node

import (
	"context"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// ListenerSource builds one carrier listener at Start. Suite construction needs material that is
// not part of a card: masking private keys, the UDP socket, or the cover profile of reality2. The
// application (or a platform layer) supplies it; the runtime starts the resulting listeners and
// closes them on Stop.
type ListenerSource interface {
	Listen(ctx context.Context) (carrier.Listener, error)
}

// Noise3Listener is the built-in noise3 ListenerSource (wire.md §2). It carries the live masking
// keys of this listener (D10: at most 8 per UDP port) and the replay cache namespace.
type Noise3Listener struct {
	// Addr is the local UDP address to bind, for example ":443" or "203.0.113.10:443".
	Addr string
	// Masks are the live masking keys (at most noise3.MaxMaskKeys).
	Masks []noise3.MaskKey
	// Profile is the traffic-shape profile published by this listener; nil means default-udp.
	Profile *shape.Profile
	// ReplayKV persists the replay filter across restarts; nil keeps it in memory.
	ReplayKV store.KV
	// LowPower disables idle padding (platforms.md §2).
	LowPower bool
	// MaxDatagramLen is the outer datagram ceiling; zero means packet.MaxDatagramLen.
	MaxDatagramLen int
	// Punch, when set, receives the listener once it is up: hole punching (T6.2) sends junk and Init
	// datagrams from this listener's socket. Pass the same value as Config.PunchNet.
	Punch *Noise3Punch
}

// Listen implements ListenerSource.
func (n Noise3Listener) Listen(ctx context.Context) (carrier.Listener, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	l, err := noise3.Listen(n.Addr, noise3.ListenerConfig{
		Keys:           n.Masks,
		KV:             n.ReplayKV,
		Profile:        n.Profile,
		LowPower:       n.LowPower,
		MaxDatagramLen: n.MaxDatagramLen,
	})
	if err != nil {
		return nil, fmt.Errorf("node: noise3 listener: %w", err)
	}
	if n.Punch != nil {
		n.Punch.attach(l)
	}
	return l, nil
}

// suiteRegistry returns the carrier registry of the runtime: the caller's, or a default with the
// dialers this build knows. Listeners are not registered by default: a listener is started by
// ListenerSource, not looked up by suite.
func suiteRegistry(cfg Config, clock carrier.Clock) (*carrier.Registry, error) {
	if cfg.Registry != nil {
		return cfg.Registry, nil
	}
	reg := carrier.NewRegistry()
	noise3.NewDialer(noise3.DialerConfig{Clock: clock}).Register(reg)
	if err := reality2.Register(reg, reality2.NewDialer(reality2.DialerConfig{Now: clock.Now}), nil); err != nil {
		return nil, fmt.Errorf("node: reality2 dialer: %w", err)
	}
	return reg, nil
}

// systemClock is the raw time source of the carrier layer. The corrected clock is Host.Clock;
// handshake timestamps add clock.Offset explicitly (handshake.md §7), so the carrier layer must
// stay raw or the correction would be applied twice.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
func (systemClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	return time.AfterFunc(d, f)
}
