package behaviour

import (
	"context"
	"os"
	"runtime"
	"time"
)

// SoakConfig is a soak: connections come and go against a server that is restarted now and then.
type SoakConfig struct {
	Duration             time.Duration
	ListenerRestartEvery int // cycles between server restarts; 0 means never
}

// SoakResult counts what the process holds at the start and at the end. A leak shows as growth.
type SoakResult struct {
	Cycles          int
	Errors          int
	GoroutinesStart int
	GoroutinesEnd   int
	HeapStart       uint64
	HeapPeak        uint64 // heap right after the last stop, before the closed listeners were let go
	HeapEnd         uint64 // heap after the process settled (up to 20 s)
	FDsStart        int    // open descriptors; counted on Linux only, 0 elsewhere
	FDsEnd          int
}

func openFDs() int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(ents)
}

func heap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// RunSoak runs connect, transfer, close cycles for cfg.Duration, restarting the server every
// ListenerRestartEvery cycles (a crash and a restart on a new port), and reports the growth of
// goroutines, descriptors and heap from the start to the end. A cycle that does not bring every
// byte back counts as an error.
func RunSoak(ctx context.Context, cfg SoakConfig) SoakResult {
	r := SoakResult{GoroutinesStart: runtime.NumGoroutine(), HeapStart: heap(), FDsStart: openFDs()}
	seed := int64(77)
	addr, stop, err := StartEcho(seed)
	if err != nil {
		r.Errors++
		return r
	}
	end := time.Now().Add(cfg.Duration)
	for time.Now().Before(end) && ctx.Err() == nil {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		t := RunTransfer(cctx, addr, seed, 16<<10, nil)
		cancel()
		r.Cycles++
		if !t.Connected || t.Echoed != t.Bytes || t.Bytes != 16<<10 {
			r.Errors++
		}
		if cfg.ListenerRestartEvery > 0 && r.Cycles%cfg.ListenerRestartEvery == 0 {
			stop()
			if addr, stop, err = StartEcho(seed); err != nil {
				r.Errors++
				break
			}
		}
	}
	stop()
	// let the closed connections finish leaving before the second count
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > r.GoroutinesStart+2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	r.HeapPeak = heap()
	// a closed listener lets go of its tables a few seconds later; the end is read after that
	r.HeapEnd = r.HeapPeak
	for settle := time.Now().Add(20 * time.Second); r.HeapEnd > r.HeapStart+16<<20 && time.Now().Before(settle); {
		time.Sleep(time.Second)
		r.HeapEnd = heap()
	}
	r.GoroutinesEnd, r.FDsEnd = runtime.NumGoroutine(), openFDs()
	return r
}
