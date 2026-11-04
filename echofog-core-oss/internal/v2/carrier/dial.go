package carrier

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Happy-eyeballs timing (wire.md §1 rule 5): the second attempt starts after
// 300 ms + uniform(0, 200) ms, or immediately when the first attempt fails.
const (
	secondAttemptDelay  = 300 * time.Millisecond
	secondAttemptJitter = 200 * time.Millisecond
)

var (
	// ErrNeedRelay means the plan has no direct carrier attempt: the link must go through a
	// relay (wire.md §1 D21 rule 4).
	ErrNeedRelay = errors.New("carrier: no direct suite, relay required")
	// ErrNoDialer means a suite has no registered Dialer. Registration is a startup action, so
	// this is a wiring error rather than a network failure.
	ErrNoDialer = errors.New("carrier: no dialer registered for suite")
)

// DialConfig configures Dial. The zero value is usable: it uses the default registry, the system
// clock, the specification's jitter and no memory.
type DialConfig struct {
	// Registry to look dialers up in; nil means DefaultRegistry.
	Registry *Registry
	// Clock is the injectable time source (AD13); nil means the system clock.
	Clock Clock
	// Jitter returns the uniform 0..200 ms part of the second-attempt delay. Nil means the
	// package default. Tests inject a fixed value.
	Jitter func() time.Duration
	// Memory, when set, reorders attempts by the suite that last worked and records the winner.
	Memory *Memory
}

// DialTimeout is the carrier dial timeout of wire.md §10 (QUIC 4 s, TCP 6 s, ws2 8 s). Unknown
// suites have no specification timeout and are bounded by ctx only.
func DialTimeout(suite uint32) time.Duration {
	switch suite {
	case objects.SuiteNoise3, objects.SuiteReality2QUIC:
		return 4 * time.Second
	case objects.SuiteReality2TCP:
		return 6 * time.Second
	case objects.SuiteWS2:
		return 8 * time.Second
	}
	return 0
}

func defaultJitter() time.Duration {
	return time.Duration(mrand.Int64N(int64(secondAttemptJitter)))
}

// Dial races the direct carrier attempts with happy eyeballs and returns the first carrier that
// connects. Attempts after the first start after the specification delay, or immediately when an
// earlier attempt fails; the first success wins and every other in-flight carrier is closed.
//
// Relay attempts are skipped; a plan with no direct attempt returns ErrNeedRelay.
func Dial(ctx context.Context, req DialRequest, attempts []Attempt, cfg DialConfig) (mux.Carrier, Attempt, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	atts := make([]Attempt, 0, len(attempts))
	for _, a := range attempts {
		if a.Kind == AttemptCarrier && a.Addr != nil {
			atts = append(atts, a)
		}
	}
	if len(atts) == 0 {
		return nil, Attempt{}, ErrNeedRelay
	}
	if cfg.Memory != nil {
		atts = cfg.Memory.prefer(ctx, atts)
	}
	registry := cfg.Registry
	if registry == nil {
		registry = defaultRegistry
	}
	clock := cfg.Clock
	if clock == nil {
		clock = systemClock{}
	}
	jitter := cfg.Jitter
	if jitter == nil {
		jitter = defaultJitter
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		c mux.Carrier
		a Attempt
		e error
	}
	// out is unbuffered so a late dial result can choose winnerCh instead of leaking a carrier.
	out := make(chan outcome)
	winnerCh := make(chan struct{})
	var winnerOnce sync.Once
	stopAll := func() {
		winnerOnce.Do(func() { close(winnerCh) })
		cancel()
	}
	defer stopAll()

	deliver := func(o outcome) {
		if o.c == nil {
			select {
			case out <- o:
			case <-winnerCh:
			}
			return
		}
		select {
		case out <- o:
		case <-winnerCh:
			_ = o.c.Close()
		}
	}

	started := 0
	done := 0
	var errs []error
	var timer Timer
	timerFor := -1
	timerCh := make(chan struct{}, 1)

	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		timerFor = -1
	}
	defer stopTimer()

	schedule := func(i int) {
		timerFor = i
		timer = clock.AfterFunc(secondAttemptDelay+jitter(), func() {
			select {
			case timerCh <- struct{}{}:
			default:
			}
		})
	}

	startAttempt := func(i int) {
		started++
		a := atts[i]
		go func() {
			d, ok := registry.DialerFor(a.Suite)
			if !ok {
				deliver(outcome{a: a, e: fmt.Errorf("%w: suite %d", ErrNoDialer, a.Suite)})
				return
			}
			dctx, dcancel := context.WithCancel(runCtx)
			var timedOut atomic.Bool
			if timeout := DialTimeout(a.Suite); timeout > 0 {
				tm := clock.AfterFunc(timeout, func() {
					timedOut.Store(true)
					dcancel()
				})
				defer tm.Stop()
			}
			r := req
			r.Addr = a.Addr
			c, err := d.Dial(dctx, r)
			dcancel()
			if err != nil {
				switch {
				case timedOut.Load() && ctx.Err() == nil && runCtx.Err() == nil:
					err = fmt.Errorf("carrier: suite %d dial timeout: %w", a.Suite, context.DeadlineExceeded)
				case ctx.Err() != nil:
					err = ctx.Err()
				default:
					err = fmt.Errorf("carrier: suite %d: %w", a.Suite, err)
				}
				deliver(outcome{a: a, e: err})
				return
			}
			deliver(outcome{c: c, a: a})
		}()
	}

	// Arm the second attempt before starting the first, so a failure can only make it earlier.
	if len(atts) > 1 {
		schedule(1)
	}
	startAttempt(0)

	for {
		select {
		case <-ctx.Done():
			return nil, Attempt{}, ctx.Err()

		case <-timerCh:
			i := timerFor
			stopTimer()
			if i >= 0 && i < len(atts) && i >= started {
				startAttempt(i)
				if started < len(atts) {
					schedule(started)
				}
			}

		case o := <-out:
			if err := ctx.Err(); err != nil {
				if o.c != nil {
					_ = o.c.Close()
				}
				return nil, Attempt{}, err
			}
			if o.e == nil {
				stopTimer()
				stopAll()
				if cfg.Memory != nil {
					// Best effort: failing to remember the winning suite must not fail a dial.
					_ = cfg.Memory.Remember(ctx, AddressKey(o.a.Addr), o.a.Suite)
				}
				return o.c, o.a, nil
			}
			done++
			errs = append(errs, o.e)
			if started < len(atts) {
				stopTimer()
				startAttempt(started)
				if started < len(atts) {
					schedule(started)
				}
				continue
			}
			if done == len(atts) {
				return nil, Attempt{}, fmt.Errorf("carrier: all %d attempts failed: %w",
					len(atts), errors.Join(errs...))
			}
		}
	}
}
