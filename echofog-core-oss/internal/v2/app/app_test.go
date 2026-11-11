package app_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/routepolicy"
	"github.com/echofog/echofog-core/internal/v2/store"
)

var testNow = time.Unix(1_800_000_000, 0)

type clock struct{}

func (clock) Now() time.Time { return testNow }

// memKV is an in-memory store.KV.
type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newKV() *memKV { return &memKV{m: map[string][]byte{}} }

func (k *memKV) Get(key []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[string(key)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}
func (k *memKV) Put(key, v []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[string(key)] = append([]byte(nil), v...)
	return nil
}
func (k *memKV) PutTTL(key, v []byte, _ time.Duration) error { return k.Put(key, v) }
func (k *memKV) Delete(key []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, string(key))
	return nil
}
func (k *memKV) Has(key []byte) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.m[string(key)]
	return ok, nil
}
func (k *memKV) Scan(prefix []byte, limit int) ([]store.Entry, error) {
	return k.ScanFrom(prefix, nil, limit)
}
func (k *memKV) ScanFrom(prefix, start []byte, limit int) ([]store.Entry, error) {
	if limit <= 0 || limit > store.MaxScanLimit { // like the real store
		return nil, store.ErrBadLimit
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	var keys []string
	for key := range k.m {
		if strings.HasPrefix(key, string(prefix)) && key > string(start) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var out []store.Entry
	for _, key := range keys {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, store.Entry{Key: []byte(key), Value: append([]byte(nil), k.m[key]...)})
	}
	return out, nil
}

type fakeRuntime struct {
	mu       sync.Mutex
	starts   int
	stops    int
	startErr error
	status   node.Status
	passes   []access.Info
	class    routepolicy.NetClass
	extended map[access.PassID]time.Duration
	revoked  []access.PassID
	rotated  int
	routes   map[node.DeviceID]node.RouteReport
}

func (f *fakeRuntime) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	return f.startErr
}
func (f *fakeRuntime) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}
func (f *fakeRuntime) RouteReport(d node.DeviceID) (node.RouteReport, bool) {
	r, ok := f.routes[d]
	return r, ok
}
func (f *fakeRuntime) Status() node.Status            { return f.status }
func (f *fakeRuntime) NetClass() routepolicy.NetClass { return f.class }
func (f *fakeRuntime) AccessList(context.Context) ([]access.Info, error) {
	return append([]access.Info(nil), f.passes...), nil
}
func (f *fakeRuntime) RevokeAccess(_ context.Context, id access.PassID) (*access.RevokeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.passes {
		if p.ID == id {
			f.revoked = append(f.revoked, id)
			p.State = access.Revoked
			return &access.RevokeResult{Pass: p}, nil
		}
	}
	return nil, access.ErrNotFound
}
func (f *fakeRuntime) ExtendAccess(_ context.Context, id access.PassID, d time.Duration) (access.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.extended == nil {
		f.extended = map[access.PassID]time.Duration{}
	}
	for _, p := range f.passes {
		if p.ID == id {
			f.extended[id] = d
			p.AccessExpires = p.AccessExpires.Add(d)
			return p, nil
		}
	}
	return access.Info{}, access.ErrNotFound
}
func (f *fakeRuntime) RotateAccessKeys(context.Context) (*access.Rotation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotated++
	return &access.Rotation{}, nil
}

type fakeLinks struct {
	contact    *links.Link
	contactErr error
	inviteOpt  access.MintOptions
	invited    int
	preview    map[string]*links.Preview
	previewErr map[string]error
	redeemed   []string
	redeemErr  error
	peer       node.Peer
}

func (f *fakeLinks) ContactLink(context.Context) (*links.Link, error) { return f.contact, f.contactErr }
func (f *fakeLinks) Invite(_ context.Context, o access.MintOptions) (*links.Invitation, error) {
	f.inviteOpt = o
	f.invited++
	return &links.Invitation{PassID: access.PassID{1, 2, 3, 4, 5, 6, 7, 8}, URL: "echofog://join?v=2&card=SECRETCARD&s=SECRETSECRET",
		Web: "https://join.example.org/join/#join?v=2&card=SECRETCARD&s=SECRETSECRET", Expires: testNow.Add(24 * time.Hour), MaxEntries: o.MaxEntries}, nil
}
func (f *fakeLinks) Open(l string) (*links.Preview, error) {
	t := strings.TrimSpace(l)
	if err, ok := f.previewErr[t]; ok {
		return nil, err
	}
	if p, ok := f.preview[t]; ok {
		return p, nil
	}
	return nil, links.ErrNotLink
}
func (f *fakeLinks) Redeem(_ context.Context, l string) (node.Peer, error) {
	f.redeemed = append(f.redeemed, l)
	if f.redeemErr != nil {
		return node.Peer{}, f.redeemErr
	}
	return f.peer, nil
}

const (
	contactURL = "echofog://join?v=2&card=AAAA"
	inviteURL  = "echofog://join?v=2&card=BBBB&s=CCCC"
)

type rig struct {
	a  *app.App
	rt *fakeRuntime
	ln *fakeLinks
	kv *memKV
}

func nid(b byte) [32]byte { var x [32]byte; x[0], x[31] = b, b; return x }

func newRig(t *testing.T, mode app.Mode) *rig {
	t.Helper()
	r := &rig{rt: &fakeRuntime{}, kv: newKV()}
	r.ln = &fakeLinks{
		contact: &links.Link{URL: "echofog://join?v=2&card=SELF", Web: "https://join.example.org/join/#join?v=2&card=SELF", Expires: testNow.Add(48 * time.Hour)},
		preview: map[string]*links.Preview{
			contactURL:                   {Kind: links.KindContact, Node: nid(7), Device: nid(8), NotAfter: testNow.Add(time.Hour)},
			inviteURL:                    {Kind: links.KindInvitation, Node: nid(9), Device: nid(10), NotAfter: testNow.Add(time.Hour)},
			"echofog://join?v=2&card=ME": {Kind: links.KindContact, Node: nid(1), Device: nid(2), NotAfter: testNow.Add(time.Hour)},
		},
		peer: node.Peer{Node: node.NodeID(nid(7)), Device: node.DeviceID(nid(8))},
	}
	r.build(t, mode)
	return r
}

func (r *rig) build(t *testing.T, mode app.Mode) {
	t.Helper()
	a, err := app.New(app.Config{Runtime: r.rt, Links: r.ln, Store: r.kv, Clock: clock{}, Self: nid(1), Mode: mode, Version: "0.3.0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.a = a
}

func TestLifecycle(t *testing.T) {
	r := newRig(t, app.ModePublic)
	ctx := context.Background()
	if r.a.Running() {
		t.Fatal("running before Start")
	}
	if err := r.a.Start(ctx); err != nil || !r.a.Running() || r.rt.starts != 1 {
		t.Fatalf("Start: %v running=%v starts=%d", err, r.a.Running(), r.rt.starts)
	}
	if err := r.a.Start(ctx); !errors.Is(err, app.ErrRunning) || r.rt.starts != 1 {
		t.Fatalf("second Start: %v starts=%d", err, r.rt.starts)
	}
	if err := r.a.Stop(ctx); err != nil || r.a.Running() || r.rt.stops != 1 {
		t.Fatalf("Stop: %v running=%v stops=%d", err, r.a.Running(), r.rt.stops)
	}
	if err := r.a.Stop(ctx); err != nil || r.rt.stops != 1 {
		t.Fatalf("Stop of a stopped app must be a no-op: %v stops=%d", err, r.rt.stops)
	}
	if err := r.a.Start(ctx); err != nil || r.rt.starts != 2 {
		t.Fatalf("restart: %v starts=%d", err, r.rt.starts)
	}
}

func TestFailedStartLeavesTheAppStopped(t *testing.T) {
	r := newRig(t, app.ModePublic)
	r.rt.startErr = errors.New("boom")
	if err := r.a.Start(context.Background()); err == nil || r.a.Running() {
		t.Fatalf("Start: %v running=%v", err, r.a.Running())
	}
	r.rt.startErr = nil
	if err := r.a.Start(context.Background()); err != nil || !r.a.Running() {
		t.Fatalf("Start after a failure: %v", err)
	}
}

func TestModeIsSavedForTheNextStart(t *testing.T) {
	r := newRig(t, app.ModePublic)
	if r.a.Mode() != app.ModePublic || r.a.NextMode() != "" {
		t.Fatalf("mode %q next %q", r.a.Mode(), r.a.NextMode())
	}
	if err := r.a.SetMode("secret"); !errors.Is(err, app.ErrMode) {
		t.Fatalf("unknown mode: %v", err)
	}
	if err := r.a.SetMode(app.ModeHidden); err != nil {
		t.Fatal(err)
	}
	if r.a.Mode() != app.ModePublic || r.a.NextMode() != app.ModeHidden {
		t.Fatalf("after SetMode: mode %q next %q", r.a.Mode(), r.a.NextMode())
	}
	// the choice survives a restart of the app, and becomes the running mode there
	r.build(t, app.ModeHidden)
	if r.a.Mode() != app.ModeHidden || r.a.NextMode() != "" {
		t.Fatalf("restarted: mode %q next %q", r.a.Mode(), r.a.NextMode())
	}
	// choosing the running mode again clears the pending change
	if err := r.a.SetMode(app.ModePublic); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SetMode(app.ModeHidden); err != nil {
		t.Fatal(err)
	}
	if r.a.NextMode() != "" {
		t.Fatalf("next mode %q, want none", r.a.NextMode())
	}
}

func TestAccessStatusOfAPublicNodeCarriesTheContactLink(t *testing.T) {
	r := newRig(t, app.ModePublic)
	st, err := r.a.AccessStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != app.ModePublic || st.Link != r.ln.contact.URL || st.WebLink != r.ln.contact.Web || st.LinkError != "" {
		t.Fatalf("status %+v", st)
	}
	r.ln.contact, r.ln.contactErr = nil, links.ErrNoContactLink
	st, err = r.a.AccessStatus(context.Background())
	if err != nil || st.Link != "" || st.LinkError == "" || strings.Contains(st.LinkError, "links:") {
		t.Fatalf("no link: %+v %v", st, err)
	}
}

func TestAccessStatusOfAHiddenNodeListsWhoCameIn(t *testing.T) {
	r := newRig(t, app.ModeHidden)
	dev := nid(40)
	r.rt.passes = []access.Info{
		{ID: access.PassID{1}, Kind: access.Invitation, State: access.Active, Created: testNow, LinkExpires: testNow.Add(time.Hour)},
		{ID: access.PassID{2}, Kind: access.Personal, State: access.Active, Created: testNow, Contact: nid(30), Device: dev},
		{ID: access.PassID{3}, Kind: access.Personal, State: access.Revoked, Created: testNow, Contact: nid(31), Device: nid(41)},
	}
	r.rt.status = node.Status{Sessions: []node.SessionStatus{{Device: node.DeviceID(dev), Path: mux.PathDirect}}}
	r.ln.contact, r.ln.contactErr = nil, links.ErrNoContactLink
	st, err := r.a.AccessStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Link != "" {
		t.Fatalf("a hidden node has no standing link: %q", st.Link)
	}
	if len(st.Joined) != 1 || !st.Joined[0].Connected {
		t.Fatalf("joined %+v: want the one active personal pass, connected", st.Joined)
	}
}

func TestInviteMapsTheRequestToTheMintOptions(t *testing.T) {
	r := newRig(t, app.ModeHidden)
	inv, err := r.a.Invite(context.Background(), app.InviteRequest{Days: 7, LinkHours: 12, Entries: 3})
	if err != nil {
		t.Fatal(err)
	}
	o := r.ln.inviteOpt
	if o.AccessTTL != 7*24*time.Hour || o.LinkTTL != 12*time.Hour || o.MaxEntries != 3 {
		t.Fatalf("mint options %+v", o)
	}
	if inv.Link == "" || inv.Entries != 3 || inv.Pass != "0102030405060708" {
		t.Fatalf("invitation %+v", inv)
	}
	for _, s := range []string{fmt.Sprint(inv), fmt.Sprintf("%+v", inv), fmt.Sprintf("%#v", inv), fmt.Sprintf("%v", &inv)} {
		if strings.Contains(s, "SECRET") || strings.Contains(s, "echofog://") {
			t.Fatalf("a formatter printed the link: %s", s)
		}
	}
}

func TestInviteLinkTermIsAMonthAtMost(t *testing.T) {
	r := newRig(t, app.ModeHidden)
	if _, err := r.a.Invite(context.Background(), app.InviteRequest{Days: 30, LinkHours: 24 * 30}); err != nil {
		t.Fatalf("a 30-day link: %v", err)
	}
	if _, err := r.a.Invite(context.Background(), app.InviteRequest{LinkHours: 24*30 + 1}); !errors.Is(err, app.ErrRequest) {
		t.Fatalf("a link longer than the card: %v, want ErrRequest", err)
	}
	if r.ln.invited != 1 {
		t.Fatalf("the refused request reached the access manager: %d", r.ln.invited)
	}
}

func TestInviteRefusals(t *testing.T) {
	pub := newRig(t, app.ModePublic)
	if _, err := pub.a.Invite(context.Background(), app.InviteRequest{Days: 1}); !errors.Is(err, app.ErrNotHidden) || pub.ln.invited != 0 {
		t.Fatalf("public node: %v invited=%d", err, pub.ln.invited)
	}
	hid := newRig(t, app.ModeHidden)
	for _, bad := range []app.InviteRequest{{Days: -1}, {LinkHours: -2}, {Days: 1e9}, {LinkHours: 1e9}, {Entries: 1 << 30}} {
		if _, err := hid.a.Invite(context.Background(), bad); !errors.Is(err, app.ErrRequest) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if hid.ln.invited != 0 {
		t.Fatal("a bad request reached the access manager")
	}
}

func TestPreviewReadsALinkInsidePastedText(t *testing.T) {
	r := newRig(t, app.ModePublic)
	info, err := r.a.Preview("Привет! Добавь меня: " + contactURL + ", жду.")
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != "contact" || info.Node != hx(nid(7)) || !info.Expires.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("info %+v", info)
	}
	inv, err := r.a.Preview(inviteURL)
	if err != nil || inv.Kind != "invitation" {
		t.Fatalf("invitation: %+v %v", inv, err)
	}
	if s := fmt.Sprintf("%+v", inv); strings.Contains(s, "CCCC") {
		t.Fatalf("the preview shows the secret: %s", s)
	}
	if _, err := r.a.Preview("просто текст"); !errors.Is(err, links.ErrNotLink) {
		t.Fatalf("not a link: %v", err)
	}
	if len(r.ln.redeemed) != 0 {
		t.Fatal("a preview dialed")
	}
}

func TestPreviewAndAddByLinkJoinAWrappedLink(t *testing.T) {
	r := newRig(t, app.ModePublic)
	// the link service is the place that joins the line breaks a terminal put inside the link
	wrapped := "echofog://join?v=2&card=" + strings.Repeat("A", 30) + "\n" + strings.Repeat("B", 30)
	r.ln.preview[wrapped] = &links.Preview{Kind: links.KindContact, Node: nid(7), Device: nid(8), NotAfter: testNow.Add(time.Hour)}

	info, err := r.a.Preview(wrapped)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if info.Kind != "contact" || info.Node != hx(nid(7)) {
		t.Fatalf("info %+v", info)
	}
	if _, err := r.a.AddByLink(context.Background(), wrapped); err != nil {
		t.Fatalf("AddByLink: %v", err)
	}
	if len(r.ln.redeemed) != 1 || r.ln.redeemed[0] != wrapped {
		t.Fatalf("redeemed %q, want the whole pasted text", r.ln.redeemed)
	}
}

// TestAWrappedLinkKeepsTheRefusalOfTheSecondPass: a link that was wrapped across lines is read by
// the second pass over the whole pasted text. When that pass refuses it for a real reason (the card
// has expired, the link is damaged), that reason must reach the UI, not "this is not a link" from
// the shortened first token.
func TestAWrappedLinkKeepsTheRefusalOfTheSecondPass(t *testing.T) {
	wrapped := "echofog://join?v=2&card=" + strings.Repeat("A", 30) + "\n" + strings.Repeat("B", 30)

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"expired", links.ErrExpired},
		{"damaged", links.ErrMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, app.ModePublic)
			r.ln.previewErr = map[string]error{wrapped: tc.err}

			if _, err := r.a.Preview(wrapped); !errors.Is(err, tc.err) {
				t.Fatalf("Preview: %v, want %v", err, tc.err)
			}
			if _, err := r.a.AddByLink(context.Background(), wrapped); !errors.Is(err, tc.err) {
				t.Fatalf("AddByLink: %v, want %v", err, tc.err)
			}
			if len(r.ln.redeemed) != 0 {
				t.Fatalf("a refused link was redeemed: %v", r.ln.redeemed)
			}
		})
	}

	// a link inside words is not a wrapped link: its first token stays the answer
	r := newRig(t, app.ModePublic)
	r.ln.previewErr = map[string]error{wrapped: links.ErrExpired}
	if _, err := r.a.Preview("вот: " + wrapped); !errors.Is(err, links.ErrNotLink) {
		t.Fatalf("text with words before the link: %v, want ErrNotLink", err)
	}
}

func TestAddByLinkSavesTheContact(t *testing.T) {
	r := newRig(t, app.ModePublic)
	c, err := r.a.AddByLink(context.Background(), "вот: "+contactURL)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ln.redeemed) != 1 || r.ln.redeemed[0] != contactURL {
		t.Fatalf("redeemed %v: only the link itself may reach the dialer", r.ln.redeemed)
	}
	if c.Node != hx(nid(7)) || c.Device != hx(nid(8)) || c.Alias == "" || !c.Added.Equal(testNow) {
		t.Fatalf("contact %+v", c)
	}
	list, err := r.a.Contacts()
	if err != nil || len(list) != 1 || list[0].Node != c.Node {
		t.Fatalf("contacts %+v %v", list, err)
	}
	// the same link again keeps the contact and the alias the user gave it
	if err := r.a.SetAlias(c.Node, "Аня"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.AddByLink(context.Background(), contactURL); err != nil {
		t.Fatal(err)
	}
	list, _ = r.a.Contacts()
	if len(list) != 1 || list[0].Alias != "Аня" {
		t.Fatalf("after a repeat: %+v", list)
	}
}

func TestAddByLinkRefusals(t *testing.T) {
	r := newRig(t, app.ModePublic)
	ctx := context.Background()
	if _, err := r.a.AddByLink(ctx, "echofog://join?v=2&card=ME"); !errors.Is(err, app.ErrSelf) || len(r.ln.redeemed) != 0 {
		t.Fatalf("own link: %v redeemed=%d", err, len(r.ln.redeemed))
	}
	if _, err := r.a.AddByLink(ctx, "нет ссылки"); !errors.Is(err, links.ErrNotLink) {
		t.Fatalf("no link: %v", err)
	}
	r.ln.redeemErr = links.ErrRevoked
	if _, err := r.a.AddByLink(ctx, inviteURL); !errors.Is(err, links.ErrRevoked) {
		t.Fatalf("revoked: %v", err)
	}
	if list, _ := r.a.Contacts(); len(list) != 0 {
		t.Fatalf("a failed join saved a contact: %+v", list)
	}
	r.ln.redeemErr = nil
	c, err := r.a.AddByLink(ctx, contactURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.Block(c.Node); err != nil {
		t.Fatal(err)
	}
	r.ln.redeemed = nil
	if _, err := r.a.AddByLink(ctx, contactURL); !errors.Is(err, app.ErrBlocked) || len(r.ln.redeemed) != 0 {
		t.Fatalf("blocked: %v redeemed=%d", err, len(r.ln.redeemed))
	}
	if err := r.a.Unblock(c.Node); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.AddByLink(ctx, contactURL); err != nil {
		t.Fatalf("after Unblock: %v", err)
	}
}

func TestAddByLinkChecksThePeerItDialed(t *testing.T) {
	r := newRig(t, app.ModePublic)
	r.ln.peer = node.Peer{Node: node.NodeID(nid(99)), Device: node.DeviceID(nid(8))}
	if _, err := r.a.AddByLink(context.Background(), contactURL); err == nil {
		t.Fatal("a session with another node became the contact of the link")
	}
	if list, _ := r.a.Contacts(); len(list) != 0 {
		t.Fatalf("saved %+v", list)
	}
}

func TestContacts(t *testing.T) {
	r := newRig(t, app.ModePublic)
	ctx := context.Background()
	a, _ := r.a.AddByLink(ctx, contactURL)
	r.ln.peer = node.Peer{Node: node.NodeID(nid(9)), Device: node.DeviceID(nid(10))}
	b, _ := r.a.AddByLink(ctx, inviteURL)
	if err := r.a.SetAlias(a.Node, "Борис"); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SetAlias(b.Node, "Аня"); err != nil {
		t.Fatal(err)
	}
	list, _ := r.a.Contacts()
	if len(list) != 2 || list[0].Alias != "Аня" || list[1].Alias != "Борис" {
		t.Fatalf("not sorted by alias: %+v", list)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("я", 65), "a\x00b", "a\nb"} {
		if err := r.a.SetAlias(a.Node, bad); !errors.Is(err, app.ErrAlias) {
			t.Errorf("alias %q: %v", bad, err)
		}
	}
	if err := r.a.SetAlias("zz", "x"); !errors.Is(err, app.ErrNoContact) {
		t.Fatalf("unknown id: %v", err)
	}
	// persisted
	r.build(t, app.ModePublic)
	if list, _ := r.a.Contacts(); len(list) != 2 {
		t.Fatalf("after restart: %+v", list)
	}
	if err := r.a.DeleteContact(a.Node); err != nil {
		t.Fatal(err)
	}
	if err := r.a.DeleteContact(a.Node); !errors.Is(err, app.ErrNoContact) {
		t.Fatalf("second delete: %v", err)
	}
	if list, _ := r.a.Contacts(); len(list) != 1 || list[0].Node != b.Node {
		t.Fatalf("after delete: %+v", list)
	}
}

func TestPasses(t *testing.T) {
	r := newRig(t, app.ModeHidden)
	r.rt.passes = []access.Info{
		{ID: access.PassID{1}, Kind: access.Invitation, State: access.Active, Created: testNow.Add(-time.Hour), LinkExpires: testNow.Add(time.Hour), MaxEntries: 2, Entries: 1},
		{ID: access.PassID{2}, Kind: access.Personal, State: access.Used, Created: testNow, AccessExpires: testNow.Add(24 * time.Hour)},
	}
	list, err := r.a.Passes(context.Background())
	if err != nil || len(list) != 2 {
		t.Fatalf("passes %+v %v", list, err)
	}
	if list[0].Kind != "personal" || list[0].State != "used" || list[1].Kind != "invitation" || list[1].State != "active" || list[1].Entries != 1 || list[1].MaxEntries != 2 {
		t.Fatalf("passes not newest first or mapped wrong: %+v", list)
	}
	id := list[0].ID
	v, err := r.a.ExtendPass(context.Background(), id, 2)
	if err != nil || r.rt.extended[access.PassID{2}] != 48*time.Hour || !v.AccessExpires.Equal(testNow.Add(72*time.Hour)) {
		t.Fatalf("extend: %+v %v %v", v, err, r.rt.extended)
	}
	for _, bad := range []float64{0, -1, 1e9} {
		if _, err := r.a.ExtendPass(context.Background(), id, bad); !errors.Is(err, app.ErrRequest) {
			t.Errorf("extend by %v days: %v", bad, err)
		}
	}
	if _, err := r.a.ExtendPass(context.Background(), "not-hex", 1); !errors.Is(err, app.ErrNoPass) {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := r.a.ExtendPass(context.Background(), "ff00000000000000", 1); !errors.Is(err, app.ErrNoPass) {
		t.Fatalf("unknown pass: %v", err)
	}
	rv, err := r.a.RevokePass(context.Background(), id)
	if err != nil || rv.State != "revoked" || len(r.rt.revoked) != 1 {
		t.Fatalf("revoke: %+v %v", rv, err)
	}
	if err := r.a.RotateAccessKeys(context.Background()); err != nil || r.rt.rotated != 1 {
		t.Fatalf("rotate: %v %d", err, r.rt.rotated)
	}
}

func sessions() []node.SessionStatus {
	mk := func(b byte, k mux.PathKind, paths int) node.SessionStatus {
		ids := make([]uint64, paths)
		return node.SessionStatus{Device: node.DeviceID(nid(b)), Node: node.NodeID(nid(b)), Path: k, Paths: ids,
			Remote: mustAddr("203.0.113.77:4433"), Suite: 1}
	}
	return []node.SessionStatus{mk(50, mux.PathDirect, 2), mk(51, mux.PathRelay, 1), mk(52, mux.PathDirect, 1)}
}

func TestNetStatusCountsSessionsAndPaths(t *testing.T) {
	r := newRig(t, app.ModeHidden)
	r.rt.status = node.Status{Sessions: sessions()}
	r.rt.class = routepolicy.NetClass(1)
	_ = r.a.Start(context.Background())
	st := r.a.NetStatus()
	if !st.Running || st.Mode != app.ModeHidden || st.Sessions != 3 || st.Direct != 2 || st.Relay != 1 || len(st.Peers) != 3 {
		t.Fatalf("status %+v", st)
	}
	if st.NetClass != routepolicy.NetClass(1).String() {
		t.Fatalf("net class %q", st.NetClass)
	}
	for _, p := range st.Peers {
		if strings.Contains(p.Device, "203.0.113") || len(p.Device) > 16 {
			t.Fatalf("peer %+v shows an address or a full id", p)
		}
	}
}

var (
	ipv4 = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	hex8 = regexp.MustCompile(`[0-9a-f]{16,}`)
)

func TestReportCarriesNoAddressesIDsOrLinks(t *testing.T) {
	r := newRig(t, app.ModeHidden)
	r.rt.status = node.Status{Sessions: sessions(), Node: node.NodeID(nid(1)), Device: node.DeviceID(nid(2))}
	r.rt.class = routepolicy.NetClass(1)
	_ = r.a.Start(context.Background())
	_, _ = r.a.AddByLink(context.Background(), contactURL)
	rep := r.a.Report()
	if !strings.HasPrefix(rep, "EchoFog connection report") {
		t.Fatalf("header: %q", rep)
	}
	for _, want := range []string{"app: 0.3.0", "protocol: 2", "mode: hidden", "sessions: 3", "direct 2", "relay 1"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	if ipv4.MatchString(rep) || hex8.MatchString(rep) || strings.Contains(rep, "echofog://") || strings.Contains(rep, "https://") {
		t.Fatalf("the report leaks an address, an id or a link:\n%s", rep)
	}
	if bytes.Contains([]byte(rep), []byte("Аня")) {
		t.Fatal("the report names a contact")
	}
}

func TestExtractLink(t *testing.T) {
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"bare", "echofog://join?v=2&card=AAAA", "echofog://join?v=2&card=AAAA", true},
		{"inside words", "смотри echofog://join?v=2&card=AAAA&s=BB, ладно?", "echofog://join?v=2&card=AAAA&s=BB", true},
		{"in brackets", "(echofog://join?v=2&card=AAAA)", "echofog://join?v=2&card=AAAA", true},
		{"quoted", `"echofog://join?v=2&card=AAAA".`, "echofog://join?v=2&card=AAAA", true},
		{"slash form", "echofog://join/?v=2&card=AAAA", "echofog://join/?v=2&card=AAAA", true},
		{"web form", "https://join.example.org/join/#join?v=2&card=AAAA!", "https://join.example.org/join/#join?v=2&card=AAAA", true},
		{"newline ends it", "echofog://join?v=2&card=AAAA\nвторая строка", "echofog://join?v=2&card=AAAA", true},
		{"first of two", "echofog://join?v=2&card=A1 echofog://join?v=2&card=A2", "echofog://join?v=2&card=A1", true},
		{"other web page", "https://example.org/page", "", false},
		{"other scheme", "http://join.example.org/join/#join?v=2&card=AAAA", "", false},
		{"nothing", "привет", "", false},
		{"empty", "", "", false},
		{"too long", "echofog://join?v=2&card=" + strings.Repeat("A", 1<<20), "", false},
	}
	for _, tc := range cases {
		got, ok := app.ExtractLink(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got %q %v, want %q %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestUserMessageIsRussianAndNeverRepeatsTheInternalText(t *testing.T) {
	for _, err := range []error{app.ErrSelf, app.ErrBlocked, app.ErrAlias, app.ErrNoContact, app.ErrMode, app.ErrNotHidden, app.ErrRequest, app.ErrNoPass, links.ErrRevoked, links.ErrNotLink, errors.New("secret internal 203.0.113.7")} {
		m := app.UserMessage(err)
		if m == "" || strings.Contains(m, "app:") || strings.Contains(m, "203.0.113") || !regexp.MustCompile(`[а-яА-Я]`).MatchString(m) {
			t.Errorf("%v -> %q", err, m)
		}
	}
}

func mustAddr(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func hx(b [32]byte) string { return hex.EncodeToString(b[:]) }

func TestReportShowsTheAppliedRuleAndTableVersion(t *testing.T) {
	r := newRig(t, app.ModePublic)
	r.rt.status = node.Status{Sessions: sessions()}
	r.rt.routes = map[node.DeviceID]node.RouteReport{
		node.DeviceID(nid(50)): {RuleID: "ru-mobile-domestic", TableVersion: "2026-11-03"},
		node.DeviceID(nid(51)): {RuleID: "ru-mobile-domestic", TableVersion: "2026-11-03"},
		node.DeviceID(nid(52)): {RuleID: "default", TableVersion: "2026-11-03"},
	}
	rep := r.a.Report()
	for _, want := range []string{"route rule ru-mobile-domestic: 2 sessions", "route rule default: 1 session", "route table: 2026-11-03"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q: %s", want, rep)
		}
	}
	if ipv4.MatchString(rep) || hex8.MatchString(rep) {
		t.Fatalf("leak: %s", rep)
	}
	r.rt.routes = nil
	if rep := r.a.Report(); !strings.Contains(rep, "route: no decision yet") {
		t.Fatalf("no decisions: %s", rep)
	}
}

func TestPolicyRefusalsHaveTheirOwnRussianText(t *testing.T) {
	seen := map[string]bool{}
	for _, err := range []error{node.ErrNoDomesticEntry, node.ErrRouteUnsupported, fmt.Errorf("dial: %w", node.ErrNoDomesticEntry)} {
		m := app.UserMessage(err)
		if m == "" || strings.Contains(m, "node:") || m == app.UserMessage(errors.New("x")) || m == app.UserMessage(links.ErrRevoked) {
			t.Errorf("%v -> %q", err, m)
		}
		seen[m] = true
	}
	if len(seen) != 2 {
		t.Fatalf("the two refusals must read differently: %v", seen)
	}
}

func TestAddByLinkKeepsAPolicyRefusalVisible(t *testing.T) {
	r := newRig(t, app.ModePublic)
	r.ln.redeemErr = fmt.Errorf("session: %w", node.ErrNoDomesticEntry)
	for _, l := range []string{contactURL, inviteURL} {
		_, err := r.a.AddByLink(context.Background(), l)
		if !errors.Is(err, node.ErrNoDomesticEntry) || app.UserMessage(err) == app.UserMessage(links.ErrRevoked) {
			t.Fatalf("%s: %v -> %q", l, err, app.UserMessage(err))
		}
	}
}

func TestReportCountsBirthdayWithoutAddresses(t *testing.T) {
	r := newRig(t, app.ModeHidden)
	r.rt.status = node.Status{Sessions: sessions(), Birthday: node.BirthdayStatus{Tried: 7, OK: 2}}
	_ = r.a.Start(context.Background())
	rep := r.a.Report()
	if !strings.Contains(rep, "birthday punch: tried 7, direct 2") {
		t.Fatalf("report lacks the birthday counts:\n%s", rep)
	}
	if ipv4.MatchString(rep) {
		t.Fatalf("the report leaks an address:\n%s", rep)
	}
}
