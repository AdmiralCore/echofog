package route

import (
	"errors"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

type fakeLink struct {
	mu    sync.Mutex
	kinds map[mux.PathKind]int
	sent  []mux.PathKind
}

func (f *fakeLink) PathKinds() map[mux.PathKind]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[mux.PathKind]int{}
	for k, v := range f.kinds {
		out[k] = v
	}
	return out
}

func (f *fakeLink) SendDatagramVia(kind mux.PathKind, typ uint64, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kinds[kind] == 0 {
		return mux.ErrNoPathKind
	}
	f.sent = append(f.sent, kind)
	return nil
}

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Route
		err  bool
	}{
		{"", Auto, false}, {"auto", Auto, false}, {" Direct ", Direct, false}, {"RELAY", Relay, false},
		{"tor", Auto, true}, {"relay,direct", Auto, true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if (err != nil) != c.err || (err == nil && got != c.want) {
			t.Errorf("Parse(%q) = %v, %v", c.in, got, err)
		}
		if err != nil && !errors.Is(err, ErrBadRoute) {
			t.Errorf("Parse(%q) error %v is not ErrBadRoute", c.in, err)
		}
	}
}

func TestChooseTable(t *testing.T) {
	d, r := mux.PathDirect, mux.PathRelay
	cases := []struct {
		name  string
		route Route
		kinds map[mux.PathKind]int
		want  mux.PathKind
		err   error
	}{
		{"auto both", Auto, map[mux.PathKind]int{d: 1, r: 1}, d, nil},
		{"auto relay only", Auto, map[mux.PathKind]int{r: 2}, r, nil},
		{"auto direct only", Auto, map[mux.PathKind]int{d: 1}, d, nil},
		{"auto none", Auto, nil, 0, ErrNoPath},
		{"direct both", Direct, map[mux.PathKind]int{d: 1, r: 1}, d, nil},
		{"direct relay only fails closed", Direct, map[mux.PathKind]int{r: 1}, 0, ErrNoPath},
		{"relay both uses relay", Relay, map[mux.PathKind]int{d: 1, r: 1}, r, nil},
		{"relay direct only fails closed", Relay, map[mux.PathKind]int{d: 3}, 0, ErrNoPath},
		{"relay zero count is no path", Relay, map[mux.PathKind]int{d: 1, r: 0}, 0, ErrNoPath},
		{"unknown route", Route(9), map[mux.PathKind]int{d: 1, r: 1}, 0, ErrBadRoute},
	}
	for _, c := range cases {
		got, err := Choose(c.route, c.kinds)
		if !errors.Is(err, c.err) || (err == nil && got != c.want) {
			t.Errorf("%s: Choose = %v, %v; want %v, %v", c.name, got, err, c.want, c.err)
		}
	}
}

// call_route=relay: whatever the paths, not one datagram leaves on a direct path.
func TestSendRelayNeverUsesDirect(t *testing.T) {
	l := &fakeLink{kinds: map[mux.PathKind]int{mux.PathDirect: 1, mux.PathRelay: 1}}
	for i := 0; i < 50; i++ {
		if _, err := Send(Relay, l, 0x11, []byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	// The relay path dies: media stops, it does not move to the direct path.
	l.mu.Lock()
	l.kinds[mux.PathRelay] = 0
	l.mu.Unlock()
	if _, err := Send(Relay, l, 0x11, []byte{1}); !errors.Is(err, ErrNoPath) {
		t.Fatalf("after the relay path died: %v, want ErrNoPath", err)
	}
	for _, k := range l.sent {
		if k != mux.PathRelay {
			t.Fatalf("a datagram left on %v", k)
		}
	}
	if len(l.sent) != 50 {
		t.Fatalf("sent %d", len(l.sent))
	}
}

func TestOnlyRelayed(t *testing.T) {
	d, r := mux.PathDirect, mux.PathRelay
	cases := []struct {
		kinds map[mux.PathKind]int
		want  bool
	}{
		{nil, false}, {map[mux.PathKind]int{d: 1}, false}, {map[mux.PathKind]int{r: 1}, true},
		{map[mux.PathKind]int{d: 1, r: 1}, false}, {map[mux.PathKind]int{r: 1, d: 0}, true},
	}
	for _, c := range cases {
		if got := OnlyRelayed(c.kinds); got != c.want {
			t.Errorf("OnlyRelayed(%v) = %v", c.kinds, got)
		}
	}
}
