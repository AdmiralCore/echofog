package node_test

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/node"
)

func TestBirthdayGatesEachCondition(t *testing.T) {
	on := node.Profile{Birthday: true}
	if g := node.BirthdayGates(on, false, false, true, true); !g.Open() {
		t.Fatalf("all conditions met but %+v is closed", g)
	}
	cases := map[string]func() bool{
		"flag off": func() bool { return node.BirthdayGates(node.Profile{}, false, false, true, true).Open() },
		"low power": func() bool {
			return node.BirthdayGates(node.Profile{Birthday: true, LowPower: true}, false, false, true, true).Open()
		},
		"guard": func() bool {
			return node.BirthdayGates(node.Profile{Birthday: true, Guard: true}, false, false, true, true).Open()
		},
		"proxy":          func() bool { return node.BirthdayGates(on, true, false, true, true).Open() },
		"anonymous":      func() bool { return node.BirthdayGates(on, false, true, true, true).Open() },
		"not noise3":     func() bool { return node.BirthdayGates(on, false, false, false, true).Open() },
		"not negotiated": func() bool { return node.BirthdayGates(on, false, false, true, false).Open() },
	}
	for name, open := range cases {
		if open() {
			t.Errorf("%s: gates open", name)
		}
	}
}

func TestDefaultProfileBirthday(t *testing.T) {
	for p, want := range map[node.Platform]bool{node.PlatformDesktop: true, node.PlatformServer: true, node.PlatformAndroid: false, node.PlatformIOS: false} {
		if got := node.DefaultProfile(p).Birthday; got != want {
			t.Errorf("platform %v: Birthday = %v, want %v", p, got, want)
		}
	}
}
