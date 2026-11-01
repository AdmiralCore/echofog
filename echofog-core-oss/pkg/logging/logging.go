// Package logging gives the whole node one place that every log line flows through: the console,
// the event bus (for the live UI), an in-memory ring of the most recent lines (so a Logs view can
// show history even if it opened late), and, optionally, a size-capped file on disk. Setup installs
// it as the default slog handler, so a plain slog.Info anywhere in the project is captured without
// the caller doing anything special.
//
// Log lines are meant to be shareable for bug reports: values of attributes whose key looks
// sensitive (password, secret, mnemonic, token, key) are redacted before they are stored.
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one captured log line, in a form easy to show in a UI or write to a file.
type Entry struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"` // "debug" | "info" | "warn" | "error"
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Line renders an entry as a single human-readable log line.
func (e Entry) Line() string {
	var b strings.Builder
	b.WriteString(e.Time.Format("15:04:05.000"))
	b.WriteByte(' ')
	b.WriteString(strings.ToUpper(e.Level))
	b.WriteByte(' ')
	b.WriteString(e.Message)
	// stable-ish order: component and name first, then the rest
	for _, k := range []string{"name", "component"} {
		if v, ok := e.Attrs[k]; ok {
			b.WriteString(" " + k + "=" + v)
		}
	}
	for k, v := range e.Attrs {
		if k == "name" || k == "component" {
			continue
		}
		b.WriteString(" " + k + "=" + v)
	}
	return b.String()
}

// ringSink is the shared store behind every Recorder derived by WithAttrs/WithGroup: the ring and
// the file live here once, so the derived handlers write to the same place.
type ringSink struct {
	mu   sync.Mutex
	ring []Entry
	next int
	full bool
	max  int

	level slog.Level // lines below this are not captured to the ring or the file

	file    *os.File
	fileMax int64
	fileLen int64
	path    string
}

func (s *ringSink) add(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring[s.next] = e
	s.next = (s.next + 1) % s.max
	if s.next == 0 {
		s.full = true
	}
	if s.file != nil {
		line := e.Line() + "\n"
		if _, err := s.file.WriteString(line); err == nil {
			s.fileLen += int64(len(line))
			if s.fileMax > 0 && s.fileLen > s.fileMax {
				s.rotateLocked()
			}
		}
	}
}

// rotateLocked keeps one previous file (.1) and starts a fresh one, so the log never grows without
// bound. The caller holds s.mu.
func (s *ringSink) rotateLocked() {
	s.file.Close()
	_ = os.Rename(s.path, s.path+".1")
	if f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		s.file = f
		s.fileLen = 0
	} else {
		s.file = nil
	}
}

func (s *ringSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
}

func (s *ringSink) recent(limit int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ordered []Entry
	if s.full {
		ordered = append(ordered, s.ring[s.next:]...)
	}
	ordered = append(ordered, s.ring[:s.next]...)
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[len(ordered)-limit:]
	}
	out := make([]Entry, len(ordered))
	copy(out, ordered)
	// lines logged at the same moment from different goroutines can reach the ring out of order:
	// order by the time each was logged, keeping arrival order for equal times
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// Recorder is an slog.Handler that writes to a shared ringSink. WithAttrs/WithGroup return a new
// Recorder pointing at the same sink, so nothing is lost across derived loggers.
type Recorder struct {
	sink   *ringSink
	attrs  []slog.Attr
	groups []string
}

func newRecorder(maxEntries int, path string, fileMax int64, level slog.Level) *Recorder {
	s := &ringSink{ring: make([]Entry, maxEntries), max: maxEntries, fileMax: fileMax, path: path, level: level}
	if path != "" {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			s.file = f
			if st, err := f.Stat(); err == nil {
				s.fileLen = st.Size()
			}
		}
	}
	return &Recorder{sink: s}
}

// Enabled honours the configured level, so lines below it (for example debug lines when the level
// is info) are never written to the shareable ring or log file.
func (r *Recorder) Enabled(_ context.Context, level slog.Level) bool { return level >= r.sink.level }

func (r *Recorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := make(map[string]string, len(r.attrs)+rec.NumAttrs())
	for _, a := range r.attrs {
		putAttr(attrs, r.groups, a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		putAttr(attrs, r.groups, a)
		return true
	})
	// The ring and file are what the Logs view shows and exports for bug reports, so mask IP
	// addresses here. The stderr console (a separate handler) keeps them for local debugging.
	r.sink.add(Entry{Time: rec.Time, Level: strings.ToLower(rec.Level.String()), Message: maskIPs(rec.Message), Attrs: attrs})
	return nil
}

func (r *Recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Recorder{sink: r.sink, attrs: append(append([]slog.Attr(nil), r.attrs...), attrs...), groups: r.groups}
}

func (r *Recorder) WithGroup(name string) slog.Handler {
	return &Recorder{sink: r.sink, attrs: r.attrs, groups: append(append([]string(nil), r.groups...), name)}
}

// Recent returns up to limit of the most recent entries, oldest first. limit <= 0 returns all.
func (r *Recorder) Recent(limit int) []Entry { return r.sink.recent(limit) }

// Close closes the log file, if any. The ring stays readable afterwards.
func (r *Recorder) Close() { r.sink.close() }

// Dump renders all retained entries as text, ready to attach to a bug report.
func (r *Recorder) Dump() string {
	var b strings.Builder
	for _, e := range r.Recent(0) {
		b.WriteString(e.Line())
		b.WriteByte('\n')
	}
	return b.String()
}

func putAttr(m map[string]string, groups []string, a slog.Attr) {
	key := a.Key
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + key
	}
	m[key] = maskIPs(redact(a.Key, fmt.Sprint(a.Value.Any())))
}

// ipToken matches things that look like an IPv4 or IPv6 address (including compressed "::" forms).
// A port, if present, follows the match and is left in place, so "203.0.113.7:4001" becomes
// "[ip]:4001" and connectivity can still be reasoned about without revealing the peer.
var ipToken = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b|(?:[0-9A-Fa-f]{0,4}:){2,}[0-9A-Fa-f]{0,4}`)

// maskIPs replaces real IP addresses in s with "[ip]". Each candidate is validated with
// net.ParseIP, so timestamps and hex IDs (which are not valid addresses) are left untouched.
func maskIPs(s string) string {
	if s == "" {
		return s
	}
	return ipToken.ReplaceAllStringFunc(s, func(m string) string {
		if net.ParseIP(m) != nil {
			return "[ip]"
		}
		return m
	})
}

// redact hides the value of an attribute whose key looks sensitive, so logs can be shared safely.
func redact(key, val string) string {
	lk := strings.ToLower(key)
	for _, bad := range []string{"password", "secret", "mnemonic", "passphrase", "token", "privkey", "private"} {
		if strings.Contains(lk, bad) {
			return "<redacted>"
		}
	}
	return val
}

// store is the process-wide recorder, set by Setup and read by the UI through Recent/Dump.
var (
	store   *Recorder
	storeMu sync.RWMutex
)

// Store returns the active recorder, or nil before Setup has run.
func Store() *Recorder {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return store
}

// Recent and Dump are convenience wrappers over the active recorder.
func Recent(limit int) []Entry {
	if r := Store(); r != nil {
		return r.Recent(limit)
	}
	return nil
}
func Dump() string {
	if r := Store(); r != nil {
		return r.Dump()
	}
	return ""
}

// Close closes the active recorder's log file (call on shutdown).
func Close() {
	if r := Store(); r != nil {
		r.Close()
	}
}

// Options configures Setup.
type Options struct {
	Level      slog.Level
	MaxEntries int            // ring size; 0 -> 5000
	FilePath   string         // "" disables the file
	FileMax    int64          // bytes before rotation; 0 -> 8 MiB
	Extra      []slog.Handler // additional handlers (e.g. the event bus)
	BaseAttrs  []slog.Attr    // attributes added to every line (e.g. the node name)
}

// Setup builds the composite handler (console + ring/file recorder + any Extra handlers), installs
// it as the default slog handler, and returns a logger using it. Call it once at start-up; after
// that, slog anywhere in the project is captured.
func Setup(o Options) *slog.Logger {
	if o.MaxEntries <= 0 {
		o.MaxEntries = 5000
	}
	if o.FileMax <= 0 {
		o.FileMax = 8 << 20
	}
	rec := newRecorder(o.MaxEntries, o.FilePath, o.FileMax, o.Level)
	storeMu.Lock()
	store = rec
	storeMu.Unlock()

	console := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: o.Level})
	handlers := append([]slog.Handler{console, rec}, o.Extra...)
	var h slog.Handler = newFanout(o.Level, handlers...)
	if len(o.BaseAttrs) > 0 {
		h = h.WithAttrs(o.BaseAttrs)
	}
	logger := slog.New(h)
	slog.SetDefault(logger)
	if o.Level < slog.LevelInfo {
		slog.SetLogLoggerLevel(o.Level)
	}
	return logger
}

// DefaultFilePath returns a per-user path for the log file, under the OS cache dir.
func DefaultFilePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "EchoFog")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "echofog.log")
}

// fanout sends every record to all handlers (the ones below the level gate drop it themselves).
type fanout struct {
	level    slog.Level
	handlers []slog.Handler
}

func newFanout(level slog.Level, handlers ...slog.Handler) *fanout {
	return &fanout{level: level, handlers: handlers}
}

func (f *fanout) Enabled(_ context.Context, level slog.Level) bool { return true }

func (f *fanout) Handle(ctx context.Context, rec slog.Record) error {
	for _, h := range f.handlers {
		if h.Enabled(ctx, rec.Level) {
			_ = h.Handle(ctx, rec.Clone())
		}
	}
	return nil
}

func (f *fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &fanout{level: f.level, handlers: hs}
}

func (f *fanout) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &fanout{level: f.level, handlers: hs}
}

// MaskIPs replaces the IP addresses in s with "[ip]", as the captured log does.
func MaskIPs(s string) string { return maskIPs(s) }
