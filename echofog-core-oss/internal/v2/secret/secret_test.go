package secret

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

// useProbe replaces the platform probe for one test and resets the cached result.
func useProbe(t *testing.T, err error) {
	t.Helper()
	old := probeFunc
	probeFunc = func() error { return err }
	resetForTest()
	t.Cleanup(func() {
		probeFunc = old
		resetForTest()
	})
}

func useBytes(t *testing.T, v *Value) []byte {
	t.Helper()
	var out []byte
	if err := v.Use(func(b []byte) error {
		out = append([]byte(nil), b...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProbeLocked(t *testing.T) {
	useProbe(t, nil)
	st := Probe()
	if st.Level != LevelLocked || st.Err != nil {
		t.Fatalf("status %+v, want locked with no error", st)
	}
}

// A platform where mlock fails must not panic: memguard panics on a refused lock, so it must
// never be reached there. The secret falls back to ordinary memory and the app is told.
func TestProbeFallbackWithoutPanic(t *testing.T) {
	useProbe(t, errors.New("mlock refused"))

	var observed []Status
	Observe(func(st Status) { observed = append(observed, st) })

	v := New([]byte("device key"))
	defer v.Destroy()

	if v.Level() != LevelOrdinary {
		t.Fatalf("level %s, want ordinary", v.Level())
	}
	if got := useBytes(t, v); string(got) != "device key" {
		t.Fatalf("bytes %q", got)
	}
	if len(observed) != 1 {
		t.Fatalf("observed %d statuses, want 1", len(observed))
	}
	if observed[0].Level != LevelOrdinary || observed[0].Err == nil ||
		!strings.Contains(observed[0].Err.Error(), "mlock refused") {
		t.Fatalf("observed %+v", observed[0])
	}

	// The warning is a one-time event, not a message per secret.
	New([]byte("another")).Destroy()
	if len(observed) != 1 {
		t.Fatalf("observed %d statuses after the second secret, want 1", len(observed))
	}
}

func TestObserveAfterProbe(t *testing.T) {
	useProbe(t, errors.New("no mlock"))
	if st := Probe(); st.Level != LevelOrdinary {
		t.Fatalf("probe %+v", st)
	}
	called := false
	Observe(func(st Status) {
		called = st.Level == LevelOrdinary && st.Err != nil
	})
	if !called {
		t.Fatal("an observer registered after the probe was not told about reduced protection")
	}
}

func TestObserveNotCalledWhenLocked(t *testing.T) {
	useProbe(t, nil)
	called := false
	Observe(func(Status) { called = true })
	Probe()
	Observe(func(Status) { called = true })
	if called {
		t.Fatal("reduced-protection observer called with locked memory")
	}
	if len(observers) != 0 {
		t.Fatalf("observers kept after a locked probe: %d", len(observers))
	}
}

func TestLockedValue(t *testing.T) {
	if st := Probe(); st.Level != LevelLocked {
		t.Skipf("this platform cannot lock pages: %v", st.Err)
	}
	v := New([]byte("account seed"))
	defer v.Destroy()
	if v.Level() != LevelLocked {
		t.Fatalf("level %s", v.Level())
	}

	// The source slice is not wiped: the caller owns it.
	src := []byte("account seed")
	New(src).Destroy()
	if !bytes.Equal(src, []byte("account seed")) {
		t.Fatal("New wiped the caller's buffer")
	}

	clone, err := v.Clone()
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Destroy()
	if !bytes.Equal(useBytes(t, clone), useBytes(t, v)) {
		t.Fatal("clone differs")
	}
	v.Destroy()
	if err := v.Use(func([]byte) error { return nil }); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("use after destroy: %v", err)
	}
	if clone.Level() != LevelLocked || string(useBytes(t, clone)) != "account seed" {
		t.Fatal("destroying the original damaged the clone")
	}
}

func TestOrdinaryMemoryIsWipedOnDestroy(t *testing.T) {
	useProbe(t, errors.New("no mlock"))
	v := New([]byte{1, 2, 3, 4, 5})
	if v.Level() != LevelOrdinary {
		t.Fatalf("level %s", v.Level())
	}
	underlying := v.plain
	v.Destroy()
	if underlying == nil {
		t.Fatal("no plain buffer to inspect")
	}
	for i, b := range underlying {
		if b != 0 {
			t.Fatalf("byte %d is %#x after destroy", i, b)
		}
	}
	v.Destroy() // idempotent
}

func TestZeroLength(t *testing.T) {
	v := New(nil)
	defer v.Destroy()
	if v.Size() != 0 {
		t.Fatalf("size %d", v.Size())
	}
	if err := v.Use(func(b []byte) error {
		if len(b) != 0 {
			t.Fatalf("len %d", len(b))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNewRandom(t *testing.T) {
	a, err := NewRandom(32)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Destroy()
	b, err := NewRandom(32)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Destroy()
	if a.Size() != 32 || b.Size() != 32 {
		t.Fatalf("sizes %d %d", a.Size(), b.Size())
	}
	ba, bb := useBytes(t, a), useBytes(t, b)
	if bytes.Equal(ba, bb) {
		t.Fatal("two random secrets are equal")
	}
	if bytes.Equal(ba, make([]byte, 32)) {
		t.Fatal("random secret is all zero")
	}
	if _, err := NewRandom(0); err == nil {
		t.Fatal("zero length accepted")
	}
}

func TestCloneDestroyed(t *testing.T) {
	v := New([]byte("x"))
	v.Destroy()
	if _, err := v.Clone(); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("clone after destroy: %v", err)
	}
}

func TestWipe(t *testing.T) {
	b := []byte{9, 8, 7}
	Wipe(b)
	for i, c := range b {
		if c != 0 {
			t.Fatalf("byte %d is %#x", i, c)
		}
	}
}

func TestUseUnderConcurrency(t *testing.T) {
	v := New([]byte("shared secret"))
	defer v.Destroy()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if err := v.Use(func(b []byte) error {
					if !bytes.Equal(b, []byte("shared secret")) {
						t.Errorf("bytes changed: %q", b)
					}
					return nil
				}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
