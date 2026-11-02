// Package perf holds the helpers of the performance measurements (the TestPerf* tests): process
// CPU time, summary statistics, a results file, and loopback forwarders that add delay or drop
// packets, so dial times and round trips can be counted at a realistic RTT.
//
// The measurements run only with ECHOFOG_PERF=1. ECHOFOG_PERF_OUT=file appends every result as a
// JSON line; ECHOFOG_PERF_REPEAT sets how many times each one is repeated (default 5).
package perf

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Skip skips the test unless ECHOFOG_PERF=1.
func Skip(t testing.TB) {
	t.Helper()
	if os.Getenv("ECHOFOG_PERF") != "1" {
		t.Skip("set ECHOFOG_PERF=1 to measure")
	}
}

// Repeat is how many times a measurement runs (ECHOFOG_PERF_REPEAT, default def).
func Repeat(def int) int { return EnvInt("ECHOFOG_PERF_REPEAT", def) }

// EnvInt reads a positive integer from the environment, def when unset or bad.
func EnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// Usage is what a piece of work cost the whole process.
type Usage struct {
	Wall time.Duration
	CPU  time.Duration // user + system time of every thread of the process
}

// CorePct is CPU as a share of one core over the wall time.
func (u Usage) CorePct() float64 {
	if u.Wall <= 0 {
		return 0
	}
	return 100 * u.CPU.Seconds() / u.Wall.Seconds()
}

// Measure runs f and returns its wall and process CPU time.
func Measure(f func()) Usage {
	c0, w0 := ProcCPU(), time.Now()
	f()
	return Usage{Wall: time.Since(w0), CPU: ProcCPU() - c0}
}

// Summary is the median and spread of repeated measurements.
type Summary struct {
	N             int
	Med, Min, Max float64
	P90, P99      float64
}

// Summarize sorts a copy of v.
func Summarize(v []float64) Summary {
	if len(v) == 0 {
		return Summary{}
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	q := func(p float64) float64 { return s[min(len(s)-1, int(p*float64(len(s))))] }
	return Summary{N: len(s), Med: q(0.5), Min: s[0], Max: s[len(s)-1], P90: q(0.9), P99: q(0.99)}
}

// String prints "median (min–max)".
func (s Summary) String() string {
	return fmt.Sprintf("%s (%s–%s)", num(s.Med), num(s.Min), num(s.Max))
}

func num(f float64) string {
	switch {
	case f >= 100:
		return strconv.FormatFloat(f, 'f', 0, 64)
	case f >= 10:
		return strconv.FormatFloat(f, 'f', 1, 64)
	default:
		return strconv.FormatFloat(f, 'f', 2, 64)
	}
}

// Durations turns durations into milliseconds.
func Durations(d []time.Duration) []float64 {
	out := make([]float64, len(d))
	for i, x := range d {
		out[i] = float64(x.Microseconds()) / 1000
	}
	return out
}

var outMu sync.Mutex

// Record logs a result and, with ECHOFOG_PERF_OUT set, appends it as a JSON line with the test,
// the commit (ECHOFOG_PERF_COMMIT) and the platform.
func Record(t testing.TB, name string, fields map[string]any) {
	t.Helper()
	b, _ := json.Marshal(fields)
	t.Logf("PERF %s %s", name, b)
	path := os.Getenv("ECHOFOG_PERF_OUT")
	if path == "" {
		return
	}
	row := map[string]any{"name": name, "test": t.Name(), "commit": os.Getenv("ECHOFOG_PERF_COMMIT"),
		"goos": runtime.GOOS, "cpus": runtime.NumCPU(), "go": runtime.Version(), "at": time.Now().UTC().Format(time.RFC3339)}
	for k, v := range fields {
		row[k] = v
	}
	line, _ := json.Marshal(row)
	outMu.Lock()
	defer outMu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Logf("perf: results file: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// Mem is a snapshot of the heap and goroutines after a GC.
type Mem struct {
	HeapInuse  uint64
	HeapAlloc  uint64
	Sys        uint64
	Goroutines int
}

// ReadMem collects garbage twice and reads the heap.
func ReadMem() Mem {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return Mem{HeapInuse: m.HeapInuse, HeapAlloc: m.HeapAlloc, Sys: m.Sys, Goroutines: runtime.NumGoroutine()}
}
