package convsync

import (
	"crypto/rand"
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// openPrefs opens the settings in a store in dir; closing it is the cleanup.
func openPrefs(t *testing.T, dir string) (*Prefs, func()) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1) // the same key each time: a reopened store reads what was written
	}
	sk := secret.New(key)
	st, err := store.Open(store.Options{Dir: dir, Key: sk})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	closeAll := func() {
		if !closed {
			closed = true
			_ = st.Close()
			sk.Destroy()
		}
	}
	t.Cleanup(closeAll)
	return NewPrefs(st.Store("v2/convsync/prefs")), closeAll
}

func TestChannelModesPersistOnTheDevice(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "v2")
	g := convlog.Group("g1")
	p, closeIt := openPrefs(t, dir)
	if got := p.Mode(g.ID, "talk"); got != ModeAll {
		t.Fatalf("the default is %v", got)
	}
	if err := p.SetMode(g.ID, "talk", ModeMentions); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMode(g.ID, "", ModeNone); err != nil {
		t.Fatal(err)
	}
	closeIt()
	p, _ = openPrefs(t, dir)
	if p.Mode(g.ID, "talk") != ModeMentions || p.Mode(g.ID, "") != ModeNone || p.Mode(g.ID, "other") != ModeAll {
		t.Fatalf("modes after reopening: %v %v %v", p.Mode(g.ID, "talk"), p.Mode(g.ID, ""), p.Mode(g.ID, "other"))
	}
	if p.Mode(convlog.Group("g2").ID, "talk") != ModeAll {
		t.Fatal("a setting leaked into another conversation")
	}
}

func TestAlertFollowsTheModeTheMentionsAndFollowedThreads(t *testing.T) {
	g := convlog.Group("g1")
	p, _ := openPrefs(t, filepath.Join(t.TempDir(), "v2"))
	root := convlog.EventID{1}
	if err := p.Follow(g.ID, root, true); err != nil {
		t.Fatal(err)
	}
	msg := func(f func(*convlog.Message)) convlog.Message {
		m := convlog.Message{Conv: g.ID.String(), Channel: "talk", Text: "x"}
		f(&m)
		return m
	}
	plain := msg(func(*convlog.Message) {})
	named := msg(func(m *convlog.Message) { m.Mentioned = true })
	inFollowed := msg(func(m *convlog.Message) { m.Thread = root.String() })
	inOther := msg(func(m *convlog.Message) { m.Thread = convlog.EventID{2}.String() })
	mine := msg(func(m *convlog.Message) { m.Mine, m.Mentioned = true, true })

	for _, c := range []struct {
		mode Mode
		m    convlog.Message
		want Alert
		name string
	}{
		{ModeAll, plain, AlertQuiet, "all/plain"},
		{ModeAll, named, AlertLoud, "all/named"},
		{ModeMentions, plain, AlertNone, "mentions/plain"},
		{ModeMentions, named, AlertLoud, "mentions/named"},
		{ModeMentions, inFollowed, AlertLoud, "mentions/followed thread"},
		{ModeMentions, inOther, AlertNone, "mentions/other thread"},
		{ModeNone, plain, AlertNone, "none/plain"},
		{ModeNone, named, AlertLoud, "none/named: being named is important even then"},
		{ModeNone, inFollowed, AlertLoud, "none/followed thread"},
		{ModeAll, inFollowed, AlertLoud, "all/followed thread"},
		{ModeAll, mine, AlertNone, "my own message"},
	} {
		if err := p.SetMode(g.ID, "talk", c.mode); err != nil {
			t.Fatal(err)
		}
		if got := p.Alert(c.m); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if err := p.Follow(g.ID, root, false); err != nil {
		t.Fatal(err)
	}
	_ = p.SetMode(g.ID, "talk", ModeMentions)
	if p.Following(g.ID, root) || p.Alert(inFollowed) != AlertNone {
		t.Fatal("a thread left is still followed")
	}
}

// The list of threads says which ones the user follows; settings change nothing in the log and send nothing.
func TestThreadListAndSettingsStayOutOfTheLog(t *testing.T) {
	c := newCluster(t, 2, 2)
	c.rosterOf(2, nil)
	a, b := c.members[0], c.members[1]
	_, root, err := a.e.Log().AppendMessageIn(c.group, "", "topic", 0, "", convlog.EventID{}, convlog.EventID{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.e.Log().AppendMessageIn(c.group, "", "a reply", 0, "", convlog.EventID{}, root); err != nil {
		t.Fatal(err)
	}
	_, other, _ := a.e.Log().AppendMessageIn(c.group, "", "another topic", 0, "", convlog.EventID{}, convlog.EventID{})
	_, _, _ = a.e.Log().AppendMessageIn(c.group, "", "reply 2", 0, "", convlog.EventID{}, other)
	_ = b

	p, _ := openPrefs(t, filepath.Join(t.TempDir(), "v2"))
	before, _ := a.e.Log().Meta(c.group)
	sent := c.n.sent
	if err := p.Follow(c.group.ID, root, true); err != nil {
		t.Fatal(err)
	}
	_ = p.SetMode(c.group.ID, "", ModeNone)
	after, _ := a.e.Log().Meta(c.group)
	if string(before.Digest()) != string(after.Digest()) || c.n.sent != sent {
		t.Fatal("a setting reached the log or the network")
	}
	list := p.Threads(a.e.Log(), c.group, "")
	if len(list) != 2 {
		t.Fatalf("threads: %+v", list)
	}
	for _, th := range list {
		if th.Following != (th.Root.ID == root.String()) {
			t.Fatalf("thread %q: following %v", th.Root.Text, th.Following)
		}
	}
	_ = rand.Reader
}
