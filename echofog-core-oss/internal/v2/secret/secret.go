// Package secret keeps the private key material of protocol v2 in memory.
//
// A secret is held in pages locked with mlock/VirtualLock (github.com/awnumar/memguard) where the
// platform allows it. Locking is probed once, before the first secret is created: memguard panics
// when a lock fails, and on Android RLIMIT_MEMLOCK is often 64 KiB, so it must not be reached
// where the kernel refuses. Without locking the bytes live in ordinary memory, are never turned
// into strings, and are wiped when the value is destroyed (platforms.md §1.2).
//
// The reduction of protection is reported once through Observe, so the app can tell the user that
// memory protection is reduced.
package secret

import (
	"crypto/rand"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/awnumar/memcall"
	"github.com/awnumar/memguard"
)

// Level is how the bytes of a secret are kept in memory.
type Level uint8

const (
	// LevelLocked means the pages are locked and cannot be swapped out.
	LevelLocked Level = iota + 1
	// LevelOrdinary means the bytes are in ordinary memory and are wiped on destroy.
	LevelOrdinary
)

func (l Level) String() string {
	switch l {
	case LevelLocked:
		return "locked"
	case LevelOrdinary:
		return "ordinary"
	default:
		return fmt.Sprintf("level(%d)", uint8(l))
	}
}

// Status is what the platform probe found.
type Status struct {
	Level Level
	// Err is why locking is unavailable; nil when Level is LevelLocked.
	Err error
}

// ErrDestroyed is returned when a destroyed value is used again.
var ErrDestroyed = errors.New("secret: value is destroyed")

// probeFunc checks that a page can be locked. It is a variable so tests can force a refusal.
var probeFunc = probeLock

// minLockBytes is the least locked-memory budget with which the process stays in locked mode:
// 256 KiB is 64 pages, enough for the device keys, prekeys and ephemeral keys a node holds at
// once.
const minLockBytes = 256 << 10

func probeLock() error {
	limit, known := lockLimit()
	if err := checkLockBudget(limit, known); err != nil {
		return err
	}

	page, err := memcall.Alloc(4096)
	if err != nil {
		return err
	}
	defer memcall.Free(page)
	if err := memcall.Lock(page); err != nil {
		return err
	}
	return memcall.Unlock(page)
}

var (
	probeOnce sync.Once
	probeMu   sync.Mutex
	probeRes  Status
	decided   bool
	observers []func(Status)
)

// Probe reports how this process can keep secrets, checking once. The check allocates one page
// and tries to lock it, which exercises the same limit memguard hits later.
func Probe() Status {
	probeOnce.Do(func() {
		st := Status{Level: LevelLocked}
		if err := probeFunc(); err != nil {
			st = Status{Level: LevelOrdinary, Err: err}
		}
		probeMu.Lock()
		probeRes, decided = st, true
		probeMu.Unlock()
	})

	probeMu.Lock()
	st := probeRes
	var obs []func(Status)
	if decided {
		obs, observers = observers, nil
	}
	probeMu.Unlock()

	if st.Level != LevelLocked {
		for _, fn := range obs {
			fn(st)
		}
	}
	return st
}

// Observe registers fn to be called when memory protection is reduced. It is meant for startup:
// if the probe has already run and the level is ordinary, fn is called before Observe returns.
// When pages can be locked, fn is never called.
func Observe(fn func(Status)) {
	if fn == nil {
		return
	}
	probeMu.Lock()
	if decided {
		st := probeRes
		probeMu.Unlock()
		if st.Level != LevelLocked {
			fn(st)
		}
		return
	}
	observers = append(observers, fn)
	probeMu.Unlock()
}

// resetForTest makes the next Probe run again. Only tests use it.
func resetForTest() {
	probeOnce = sync.Once{}
	probeMu.Lock()
	probeRes, decided, observers = Status{}, false, nil
	probeMu.Unlock()
}

// Value is a secret in memory. Its bytes are read only inside Use; copies made there must not
// outlive the callback.
type Value struct {
	mu     sync.Mutex
	level  Level
	locked *memguard.LockedBuffer
	plain  []byte
	dead   bool
}

// New returns a Value holding a copy of b at the best level this process can provide. The caller
// keeps ownership of b: New does not wipe it.
func New(b []byte) *Value {
	return NewAt(Probe().Level, b)
}

// NewAt returns a Value at level, but never locked when the platform probe did not succeed.
func NewAt(level Level, b []byte) *Value {
	if level != LevelLocked || Probe().Level != LevelLocked || len(b) == 0 {
		return &Value{level: LevelOrdinary, plain: append([]byte(nil), b...)}
	}
	buf := memguard.NewBuffer(len(b))
	buf.Copy(b)
	buf.Freeze()
	return &Value{level: LevelLocked, locked: buf}
}

// NewRandom returns a Value holding n random bytes.
func NewRandom(n int) (*Value, error) {
	if n <= 0 {
		return nil, fmt.Errorf("secret: random length %d", n)
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	defer Wipe(b)
	return New(b), nil
}

// Level reports how this value is kept.
func (v *Value) Level() Level {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dead {
		return LevelOrdinary
	}
	return v.level
}

// Size returns the length of the secret in bytes.
func (v *Value) Size() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dead {
		return 0
	}
	if v.locked != nil {
		return v.locked.Size()
	}
	return len(v.plain)
}

// Use calls fn with the bytes of the secret. The slice is valid only for the call: fn must not
// retain or modify it, and must not call back into v. Use waits while another call runs.
func (v *Value) Use(fn func([]byte) error) error {
	if fn == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dead {
		return ErrDestroyed
	}
	if v.locked != nil {
		return fn(v.locked.Bytes())
	}
	return fn(v.plain)
}

// Clone returns an independent copy at the same level.
func (v *Value) Clone() (*Value, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dead {
		return nil, ErrDestroyed
	}
	if v.locked != nil {
		return NewAt(LevelLocked, v.locked.Bytes()), nil
	}
	return NewAt(LevelOrdinary, v.plain), nil
}

// Destroy wipes the bytes and frees locked pages. It is idempotent; a destroyed Value cannot be
// used again.
func (v *Value) Destroy() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dead {
		return
	}
	v.dead = true
	if v.locked != nil {
		v.locked.Destroy()
		v.locked = nil
	}
	Wipe(v.plain)
	v.plain = nil
}

// Wipe overwrites b with zeroes. The compiler keeps the write alive even though b is not read
// afterwards.
func Wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}
