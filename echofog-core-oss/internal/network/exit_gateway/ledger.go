package exit_gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Money on an exit is counted in micro-USDT (1 USDT = 1_000_000). The price is micro-USDT per MB,
// so 5 cents a GB is about 49. A payment is turned into bytes at the price in force when it is
// credited; from then on only bytes are tracked.
const (
	bytesPerMB = 1 << 20

	// OverdraftBytes lets a client that has paid before run slightly past zero while its next
	// payment is still confirming on-chain (about 10-30 s). It is repaid from that payment. A
	// first-time client gets no overdraft: it must have paid once.
	OverdraftBytes = 64 << 20

	// MinPayMicro is the smallest transfer an exit credits; anything below is network dust.
	MinPayMicro = 10_000 // 0.01 USDT

	// ledgerKeep is how long processed transfers are remembered. Older ones are ignored when
	// they show up in the wallet history, so nothing can be credited twice.
	ledgerKeep = 14 * 24 * time.Hour

	ledgerFlushEvery = 5 * time.Second
)

// PayTag is the memo a client puts on its transfer so the exit knows whom to credit. It is
// derived from both peer IDs: the exit can recompute it for any connected peer without storing a
// registry, and an observer of the blockchain cannot tie it to a peer ID without already
// knowing which two peers talk.
func PayTag(client, exit types.PeerID) string {
	h := sha256.New()
	h.Write([]byte("echofog/pay/v1"))
	h.Write(client[:])
	h.Write(exit[:])
	return "ef1:" + hex.EncodeToString(h.Sum(nil)[:8])
}

type ledgerAccount struct {
	CreditBytes int64 `json:"credit"` // paid bytes left; negative = overdraft owed
	EverPaid    bool  `json:"paid"`   // has at least one credited payment
	PaidMicro   int64 `json:"paid_micro"`
	Updated     int64 `json:"updated"`
}

type ledgerFile struct {
	Accounts map[string]*ledgerAccount `json:"accounts"`
	Seen     map[string]int64          `json:"seen"` // transfer hash -> unix seconds
	Earned   int64                     `json:"earned_micro"`
}

// Ledger holds what each tag has paid for and what is left of it. It survives restarts.
type Ledger struct {
	mu    sync.Mutex
	path  string
	data  ledgerFile
	dirty bool
	now   func() time.Time
	stop  chan struct{}
	done  chan struct{}
}

// NewLedger loads the ledger from path ("" keeps it in memory only).
func NewLedger(path string) *Ledger {
	l := &Ledger{path: path, now: time.Now}
	l.data.Accounts = make(map[string]*ledgerAccount)
	l.data.Seen = make(map[string]int64)
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			var f ledgerFile
			if json.Unmarshal(raw, &f) == nil {
				if f.Accounts != nil {
					l.data.Accounts = f.Accounts
				}
				if f.Seen != nil {
					l.data.Seen = f.Seen
				}
				l.data.Earned = f.Earned
			}
		}
	}
	return l
}

// Start saves consumption in the background, so a per-packet balance update never touches disk.
func (l *Ledger) Start() {
	l.mu.Lock()
	if l.stop != nil {
		l.mu.Unlock()
		return
	}
	l.stop, l.done = make(chan struct{}), make(chan struct{})
	stop, done := l.stop, l.done
	l.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(ledgerFlushEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = l.Flush()
			case <-stop:
				_ = l.Flush()
				return
			}
		}
	}()
}

// Close stops the background saver and writes the final state.
func (l *Ledger) Close() {
	l.mu.Lock()
	stop, done := l.stop, l.done
	l.stop, l.done = nil, nil
	l.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	_ = l.Flush()
}

func (l *Ledger) account(tag string) *ledgerAccount {
	a := l.data.Accounts[tag]
	if a == nil {
		a = &ledgerAccount{}
		l.data.Accounts[tag] = a
	}
	return a
}

// Credit books one incoming transfer. It reports false for a transfer already booked.
func (l *Ledger) Credit(tag, txHash string, micro, pricePerMB int64) (bytes int64, booked bool) {
	if pricePerMB <= 0 || micro < MinPayMicro || tag == "" || txHash == "" {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.data.Seen[txHash]; dup {
		return 0, false
	}
	now := l.now()
	l.data.Seen[txHash] = now.Unix()
	l.pruneLocked(now)

	bytes = mulDiv(micro, bytesPerMB, pricePerMB)
	a := l.account(tag)
	a.CreditBytes += bytes
	a.EverPaid = true
	a.PaidMicro += micro
	a.Updated = now.Unix()
	l.data.Earned += micro
	l.dirty = true
	_ = l.flushLocked() // money in: never leave it to the 5 s timer
	return bytes, true
}

// Seen reports whether a transfer was already booked, or is too old to be considered.
func (l *Ledger) Seen(txHash string, when time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.now().Sub(when) > ledgerKeep {
		return true
	}
	_, ok := l.data.Seen[txHash]
	return ok
}

// Balance returns the paid bytes left for tag and whether it has ever paid.
func (l *Ledger) Balance(tag string) (credit int64, everPaid bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.data.Accounts[tag]; a != nil {
		return a.CreditBytes, a.EverPaid
	}
	return 0, false
}

// Earned is the total ever credited, in micro-USDT.
func (l *Ledger) Earned() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.data.Earned
}

// Consume spends n bytes of paid credit and reports whether the tag may keep going. A tag that has
// never paid cannot go below zero; one that has may overdraw by OverdraftBytes.
func (l *Ledger) Consume(tag string, n int64) bool {
	if n <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.data.Accounts[tag]
	if a == nil {
		return false
	}
	floor := int64(0)
	if a.EverPaid {
		floor = -OverdraftBytes
	}
	if a.CreditBytes-n < floor {
		// not enough: spend what is allowed, then refuse
		if a.CreditBytes > floor {
			a.CreditBytes = floor
			l.dirty = true
		}
		return false
	}
	a.CreditBytes -= n
	l.dirty = true
	return true
}

// Reserve takes up to n bytes of tag's paid credit (overdraft included) and returns how many it
// got: a stream sets them aside and spends them without coming back here for every read.
func (l *Ledger) Reserve(tag string, n int64) int64 {
	if n <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.data.Accounts[tag]
	if a == nil {
		return 0
	}
	floor := int64(0)
	if a.EverPaid {
		floor = -OverdraftBytes
	}
	got := min(n, a.CreditBytes-floor)
	if got <= 0 {
		return 0
	}
	a.CreditBytes -= got
	l.dirty = true
	return got
}

// Refund gives back reserved bytes a stream did not use.
func (l *Ledger) Refund(tag string, n int64) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.data.Accounts[tag]; a != nil {
		a.CreditBytes += n
		l.dirty = true
	}
}

// Available is the number of bytes tag could still spend right now (paid credit plus overdraft).
func (l *Ledger) Available(tag string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.data.Accounts[tag]
	if a == nil {
		return 0
	}
	floor := int64(0)
	if a.EverPaid {
		floor = -OverdraftBytes
	}
	if v := a.CreditBytes - floor; v > 0 {
		return v
	}
	return 0
}

func (l *Ledger) pruneLocked(now time.Time) {
	cut := now.Add(-ledgerKeep).Unix()
	for h, ts := range l.data.Seen {
		if ts < cut {
			delete(l.data.Seen, h)
		}
	}
	// forget idle accounts that owe nothing and hold nothing
	for t, a := range l.data.Accounts {
		if a.CreditBytes == 0 && a.Updated < cut {
			delete(l.data.Accounts, t)
		}
	}
}

// Flush writes the ledger if it changed.
func (l *Ledger) Flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flushLocked()
}

func (l *Ledger) flushLocked() error {
	if l.path == "" || !l.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(&l.data)
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	l.dirty = false
	return nil
}

// mulDiv computes a*b/c without overflowing for the ranges used here.
func mulDiv(a, b, c int64) int64 {
	if c <= 0 {
		return 0
	}
	hi := a / c
	lo := a % c
	return hi*b + lo*b/c
}
