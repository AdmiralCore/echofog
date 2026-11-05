package relay

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// AD20 (T6.1 H1′, accept 2): every "unavailable" outcome — no target, silent, refusing, busy —
// leaves the requester the same observable: the same answer, after the same delay, with the same
// slots held until that answer. Tests below are the table of causes, sequential and N+1 parallel.

type ad20Cause struct {
	name  string
	setup func(t *testing.T, w *world, a *side) [32]byte // returns the target ID to probe
}

func ad20Causes() []ad20Cause {
	return []ad20Cause{
		{"absent", func(t *testing.T, w *world, a *side) [32]byte {
			var id [32]byte
			id[0] = 9
			return id
		}},
		{"silent", func(t *testing.T, w *world, a *side) [32]byte {
			b := w.join(newTNode(t), "10.0.1.2", true)
			w.mu.Lock()
			w.silent[b.node] = true
			w.mu.Unlock()
			return b.node
		}},
		{"refusing", func(t *testing.T, w *world, a *side) [32]byte {
			b := w.join(newTNode(t), "10.0.1.3", true)
			b.pol = func(Incoming) bool { return false }
			return b.node
		}},
		{"unlisted", func(t *testing.T, w *world, a *side) [32]byte {
			return w.join(newTNode(t), "10.0.1.4", false).node
		}},
		{"busy", func(t *testing.T, w *world, a *side) [32]byte {
			b := w.join(newTNode(t), "10.0.1.5", true)
			for i, ip := range []string{"10.0.2.1", "10.0.2.2"} {
				x := w.join(newTNode(t), ip, true)
				if _, err := x.dial(b); err != nil {
					t.Fatalf("filler %d: %v", i, err)
				}
			}
			return b.node
		}},
	}
}

func ad20World(t *testing.T, delay time.Duration, lim Limits) *world {
	return newWorld(t, ServerConfig{Delay: func() time.Duration { return delay }, Limits: lim})
}

func (w *world) activeSlots() int {
	w.srv.mu.Lock()
	defer w.srv.mu.Unlock()
	return w.srv.active
}

func reasonOf(t *testing.T, err error) uint32 {
	t.Helper()
	var re *RefusedError
	if !errors.As(err, &re) {
		t.Fatalf("got %v, want a REFUSED", err)
	}
	return re.Reason
}

// One request: the answer is "unavailable" after the delay, and the requester's slot is still
// held half-way to it (it is returned only after the answer), whatever the cause.
func TestUnavailableCausesSequential(t *testing.T) {
	const delay = 800 * time.Millisecond
	for _, c := range ad20Causes() {
		t.Run(c.name, func(t *testing.T) {
			w := ad20World(t, delay, Limits{PerSession: 2})
			a := w.join(newTNode(t), "10.0.0.1", true)
			target := c.setup(t, w, a)
			base := w.activeSlots() // busy: the fillers' circuits
			start := time.Now()
			done := make(chan error, 1)
			go func() { _, err := Dial(w.ctx, a.open, w.rNode, target, ConnectOptions{}); done <- err }()
			time.Sleep(delay / 2)
			if got := w.activeSlots() - base; got != 1 {
				t.Errorf("requester slots held at half the delay: %d, want 1", got)
			}
			err := <-done
			if r := reasonOf(t, err); r != ReasonUnavailable {
				t.Errorf("reason %d, want unavailable", r)
			}
			if d := time.Since(start); d < delay-50*time.Millisecond || d > delay+250*time.Millisecond {
				t.Errorf("answered after %v, want about %v", d, delay)
			}
			waitFor(t, func() bool { return w.activeSlots() == base })
		})
	}
}

// N+1 parallel requests (here 2+2) at one limit: two take their slots and hold them until the
// answer, the other two get "limit" — the same count for every cause of "unavailable", for each
// of the three limits. Before the fix an absent target gave back its slot at once and nobody
// saw "limit".
func TestUnavailableCausesParallelLimits(t *testing.T) {
	const delay = 700 * time.Millisecond
	const budget, extra = 2, 2
	for _, kind := range []string{"PerSession", "PerIP", "Total"} {
		for _, c := range ad20Causes() {
			t.Run(kind+"/"+c.name, func(t *testing.T) {
				fill := 0
				if c.name == "busy" {
					fill = 2
				}
				lim := Limits{PerSession: 2}
				switch kind {
				case "PerIP":
					lim.PerIP = budget
				case "Total":
					lim.Total = budget + fill
				}
				w := ad20World(t, delay, lim)
				reqs := make([]*side, budget+extra)
				for i := range reqs {
					switch kind {
					case "PerSession":
						if i > 0 {
							reqs[i] = reqs[0]
							continue
						}
						reqs[i] = w.join(newTNode(t), "10.0.0.1", true)
					case "PerIP":
						reqs[i] = w.join(newTNode(t), "10.0.0.1", true)
					default:
						reqs[i] = w.join(newTNode(t), "10.0.0."+string(rune('1'+i))+"0", true)
					}
				}
				target := c.setup(t, w, reqs[0])
				var mu sync.Mutex
				counts := map[uint32]int{}
				var wg sync.WaitGroup
				for _, r := range reqs {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, err := Dial(w.ctx, r.open, w.rNode, target, ConnectOptions{})
						mu.Lock()
						counts[reasonOf(t, err)]++
						mu.Unlock()
					}()
				}
				wg.Wait()
				if counts[ReasonLimit] != extra || counts[ReasonUnavailable] != budget {
					t.Errorf("answers %v, want %d limit and %d unavailable", counts, extra, budget)
				}
			})
		}
	}
}
