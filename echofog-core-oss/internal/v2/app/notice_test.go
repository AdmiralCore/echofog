package app_test

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/release"
)

var cyrillic = regexp.MustCompile(`[А-Яа-яЁё]`)

// collect subscribes and returns the notices received so far.
func collect(r *rig) (*[]app.Notice, func()) {
	var got []app.Notice
	cancel := r.a.Subscribe(func(n app.Notice) { got = append(got, n) })
	return &got, cancel
}

func updateEvent(required bool, notes string) node.Event {
	return node.Event{Kind: node.EventUpdateAvailable, Update: &release.Event{
		Version: "0.3.1", NotesURL: notes, Required: required,
		Components: []release.ComponentStatus{{Component: "proto", Own: 200}},
	}}
}

func TestUpdateEventBecomesANotice(t *testing.T) {
	r := newRig(t, app.ModePublic)
	got, _ := collect(r)
	r.a.Sink().Emit(updateEvent(false, "https://example.org/notes"))
	if len(*got) != 1 || (*got)[0].Kind != app.NoticeUpdate || (*got)[0].Update == nil {
		t.Fatalf("notices: %+v", *got)
	}
	u := (*got)[0].Update
	if u.Version != "0.3.1" || u.NotesURL != "https://example.org/notes" || u.Important || len(u.Components) != 1 || u.Components[0] != "proto" {
		t.Fatalf("update: %+v", u)
	}
	if !cyrillic.MatchString((*got)[0].Text) || !strings.Contains((*got)[0].Text, "0.3.1") {
		t.Fatalf("text: %q", (*got)[0].Text)
	}
	if last := r.a.LastUpdate(); last == nil || last.Version != "0.3.1" {
		t.Fatalf("LastUpdate: %+v", last)
	}
}

func TestRequiredUpdateIsMarkedImportant(t *testing.T) {
	r := newRig(t, app.ModePublic)
	got, _ := collect(r)
	r.a.Sink().Emit(updateEvent(true, "https://example.org/n"))
	if len(*got) != 1 || !(*got)[0].Update.Important {
		t.Fatalf("notices: %+v", *got)
	}
	plain := newRig(t, app.ModePublic)
	got2, _ := collect(plain)
	plain.a.Sink().Emit(updateEvent(false, "https://example.org/n"))
	if (*got)[0].Text == (*got2)[0].Text {
		t.Fatal("an important update reads like an ordinary one")
	}
}

// A page that is not https never reaches the browser, whatever the manifest says.
func TestUpdateNotesURLMustBeHTTPS(t *testing.T) {
	for _, bad := range []string{"http://example.org/n", "javascript:alert(1)", "file:///c:/x", "//example.org/n", "ftp://x/y", " https://example.org/n"} {
		r := newRig(t, app.ModePublic)
		got, _ := collect(r)
		r.a.Sink().Emit(updateEvent(false, bad))
		if len(*got) != 1 || (*got)[0].Update.NotesURL != "" {
			t.Errorf("%q: notices %+v", bad, *got)
		}
	}
}

func TestUpdateEventWithoutPayloadIsIgnored(t *testing.T) {
	r := newRig(t, app.ModePublic)
	got, _ := collect(r)
	r.a.Sink().Emit(node.Event{Kind: node.EventUpdateAvailable})
	if len(*got) != 0 || r.a.LastUpdate() != nil {
		t.Fatalf("notices: %+v", *got)
	}
}

func TestPolicyEventsReachTheUI(t *testing.T) {
	r := newRig(t, app.ModePublic)
	got, _ := collect(r)
	r.a.Sink().Emit(node.Event{Kind: node.EventNoDomesticEntry, Err: node.ErrNoDomesticEntry})
	r.a.Sink().Emit(node.Event{Kind: node.EventNoDomesticEntry, Err: fmt.Errorf("wrapped: %w", node.ErrRouteUnsupported)})
	if len(*got) != 2 {
		t.Fatalf("notices: %+v", *got)
	}
	a, b := (*got)[0], (*got)[1]
	if a.Kind != app.NoticeNoDomesticEntry || b.Kind != app.NoticeNoDomesticEntry {
		t.Fatalf("kinds: %v %v", a.Kind, b.Kind)
	}
	if a.Text != app.UserMessage(node.ErrNoDomesticEntry) || b.Text != app.UserMessage(node.ErrRouteUnsupported) || a.Text == b.Text {
		t.Fatalf("texts: %q / %q", a.Text, b.Text)
	}
}

// The clock event of T3.8-fix2 is not in this branch's node package yet; the sink knows its kind
// by name, so the UI works the day the event lands.
func TestClockSkewEventHasAnExtensionPoint(t *testing.T) {
	r := newRig(t, app.ModePublic)
	got, _ := collect(r)
	r.a.Sink().Emit(node.Event{Kind: "node.clock-skew"})
	if len(*got) != 1 || (*got)[0].Kind != app.NoticeClockSkew {
		t.Fatalf("notices: %+v", *got)
	}
	if !strings.Contains((*got)[0].Text, "Часы") || strings.Contains((*got)[0].Text, "node.") {
		t.Fatalf("text: %q", (*got)[0].Text)
	}
}

func TestReleaseUnavailableAndNoCoverEvents(t *testing.T) {
	r := newRig(t, app.ModePublic)
	got, _ := collect(r)
	r.a.Sink().Emit(node.Event{Kind: node.EventReleaseUnavailable, Err: errors.New("db at /home/u/x broke")})
	r.a.Sink().Emit(node.Event{Kind: node.EventNoCover, Message: "node 10.1.2.3 has no cover"})
	if len(*got) != 2 || (*got)[0].Kind != app.NoticeReleaseUnavailable || (*got)[1].Kind != app.NoticeNoCover {
		t.Fatalf("notices: %+v", *got)
	}
	for _, n := range *got {
		if !cyrillic.MatchString(n.Text) || strings.Contains(n.Text, "10.1.2.3") || strings.Contains(n.Text, "/home") {
			t.Errorf("%v text %q repeats internals", n.Kind, n.Text)
		}
	}
}

func TestOtherEventsAreNotNotices(t *testing.T) {
	r := newRig(t, app.ModePublic)
	got, _ := collect(r)
	for _, k := range []node.EventKind{node.EventClockWarning, node.EventNetworkChanged, node.EventPublicAddr, node.EventPortMapping, "node.nonsense"} {
		r.a.Sink().Emit(node.Event{Kind: k, Message: "10.0.0.1"})
	}
	if len(*got) != 0 {
		t.Fatalf("notices: %+v", *got)
	}
}

func TestSubscribeCancelAndManySubscribers(t *testing.T) {
	r := newRig(t, app.ModePublic)
	a, cancelA := collect(r)
	b, _ := collect(r)
	r.a.Sink().Emit(updateEvent(false, ""))
	cancelA()
	cancelA() // twice is fine
	r.a.Sink().Emit(updateEvent(false, ""))
	if len(*a) != 1 || len(*b) != 2 {
		t.Fatalf("a=%d b=%d", len(*a), len(*b))
	}
}

// A handler may unsubscribe or subscribe from inside a notice without a deadlock.
func TestSubscribeFromInsideAHandler(t *testing.T) {
	r := newRig(t, app.ModePublic)
	var cancel func()
	calls := 0
	cancel = r.a.Subscribe(func(app.Notice) {
		calls++
		cancel()
		r.a.Subscribe(func(app.Notice) {})
	})
	r.a.Sink().Emit(updateEvent(false, ""))
	r.a.Sink().Emit(updateEvent(false, ""))
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
}

func TestErrorScreens(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"link of another version", links.ErrOtherVersion, "other-version"},
		{"wrapped", fmt.Errorf("add: %w", links.ErrOtherVersion), "other-version"},
		{"server without a cover", &exit.Refusal{Reason: exit.ReasonNoCamouflage}, "no-cover"},
		{"free volume used", app.ErrFreeQuotaUsed, "free-quota-used"},
		{"wrapped quota", fmt.Errorf("exit: %w", app.ErrFreeQuotaUsed), "free-quota-used"},
	}
	seen := map[string]app.ErrorScreen{}
	for _, c := range cases {
		s, ok := app.Screen(c.err)
		if !ok || s.Code != c.code {
			t.Errorf("%s: %+v ok=%v", c.name, s, ok)
			continue
		}
		if !cyrillic.MatchString(s.Title) || !cyrillic.MatchString(s.Text) || s.Title == s.Text {
			t.Errorf("%s: texts %+v", c.name, s)
		}
		if strings.Contains(s.Text, "exit refused") || strings.Contains(s.Text, "app:") || strings.Contains(s.Text, "links:") {
			t.Errorf("%s: internal text leaks: %q", c.name, s.Text)
		}
		seen[s.Code] = s
	}
	if len(seen) != 3 {
		t.Fatalf("three screens expected, got %v", seen)
	}
	for _, e := range []error{nil, errors.New("boom"), links.ErrRevoked, &exit.Refusal{Reason: exit.ReasonBusy}} {
		if s, ok := app.Screen(e); ok {
			t.Errorf("%v has a screen: %+v", e, s)
		}
	}
	for code := range seen {
		if s, ok := app.ScreenByCode(code); !ok || s.Code != code {
			t.Errorf("ScreenByCode(%q): %+v %v", code, s, ok)
		}
	}
	if _, ok := app.ScreenByCode("nonsense"); ok {
		t.Error("an unknown code has a screen")
	}
}

// The words of a screen are neutral: no verbs in the past tense about the user.
func TestScreenTextsAreNeutral(t *testing.T) {
	for _, code := range []string{"other-version", "no-cover", "free-quota-used"} {
		s, ok := app.ScreenByCode(code)
		if !ok {
			t.Fatalf("no screen %q", code)
		}
		for _, bad := range []string{"вы ", "Вы ", "ваш", "Ваш", "сделал", "нажал", "ввёл"} {
			if strings.Contains(s.Title+" "+s.Text, bad) {
				t.Errorf("%s: %q in %q", code, bad, s.Title+" "+s.Text)
			}
		}
	}
}
