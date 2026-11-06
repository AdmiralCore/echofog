package links_test

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/store"
)

var testNow = time.Unix(1_800_000_000, 0)

const testPhrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

const joinHost = "join.example.org"

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// rig is one node: a card builder, and an access manager when the node is hidden.
type rig struct {
	clk     *fakeClock
	account *identity.Account
	device  *identity.Device
	builder *cards.CardBuilder
	mgr     *access.Manager
}

var (
	sharedMu   sync.Mutex
	sharedRigs = map[cards.Visibility]*rig{}
	cleanups   []func()
)

// TestMain destroys the shared nodes: many identities in one Windows test binary exhaust the
// memguard locked-memory quota, so every test shares one visible and one hidden node.
func TestMain(m *testing.M) {
	code := m.Run()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
	os.Exit(code)
}

func newRig(t *testing.T, vis cards.Visibility) *rig {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if r := sharedRigs[vis]; r != nil {
		return r
	}
	r := buildRig(t, vis)
	sharedRigs[vis] = r
	return r
}

func buildRig(t *testing.T, vis cards.Visibility) *rig {
	clk := &fakeClock{now: testNow}
	account, err := identity.FromMnemonic(testPhrase, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	device, err := identity.NewDevice()
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	devices, err := account.SealSingleDeviceList(device, 1, clk.Now())
	if err != nil {
		t.Fatalf("device list: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	val := secret.New(key)
	dir, err := os.MkdirTemp("", "links-test-")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(store.Options{Dir: dir, Key: val})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	cleanups = append(cleanups, func() {
		_ = st.Close()
		val.Destroy()
		account.Destroy()
		device.Destroy()
		_ = os.RemoveAll(dir)
	})

	l := cards.Listener{Suite: objects.SuiteReality2QUIC, Host: "203.0.113.10", Port: 443, SNI: "example.com", Mask: device.Masking().Public()}
	if vis == cards.Hidden {
		l = cards.Listener{Suite: objects.SuiteNoise3, Host: "198.51.100.20", Port: 5555, Mask: device.Masking().Public()}
	}
	b, err := cards.New(cards.Config{
		Account: account.Public(), Device: device, HsKey: device.Handshake().Public(),
		Role: objects.RoleUser, Listeners: []cards.Listener{l}, Devices: devices,
		Release: "0.3.0", Visibility: vis, Clock: clk,
	})
	if err != nil {
		t.Fatalf("card builder: %v", err)
	}
	r := &rig{clk: clk, account: account, device: device, builder: b}
	if vis == cards.Hidden {
		// Three listener slots: eight keys each, so the tests never hit the key limit.
		slots := []cards.Listener{
			{Suite: objects.SuiteNoise3, Host: "203.0.113.10", Port: 4242},
			{Suite: objects.SuiteNoise3, Host: "203.0.113.10", Port: 4243},
			{Suite: objects.SuiteNoise3, Host: "203.0.113.10", Port: 4244},
		}
		m, err := access.New(access.Config{
			KV: st.Store(access.Namespace), Cards: b, Clock: clk, ContactsKey: device.Masking(), Listeners: slots,
		})
		if err != nil {
			t.Fatalf("access: %v", err)
		}
		cleanups = append(cleanups, m.Close)
		r.mgr = m
	}
	return r
}

// service is a link service over the rig with its own clock; the rig's cards stay valid when a
// test moves the service clock.
func (r *rig) service(t *testing.T, d links.Dialer) *links.Service {
	t.Helper()
	return r.serviceAt(t, d, &fakeClock{now: testNow})
}

func (r *rig) serviceAt(t *testing.T, d links.Dialer, clk *fakeClock) *links.Service {
	t.Helper()
	cfg := links.Config{Cards: r.builder, Dialer: d, Clock: clk, JoinHost: joinHost}
	if r.mgr != nil {
		cfg.Inviter = r.mgr
	}
	s, err := links.New(cfg)
	if err != nil {
		t.Fatalf("links.New: %v", err)
	}
	return s
}

func (r *rig) ids() (nodeID, deviceID [32]byte) {
	return r.account.NodeID(), r.device.ID()
}

type dialCall struct {
	peer node.DeviceID
	opt  node.DialOpt
}

type fakeDialer struct {
	mu    sync.Mutex
	calls []dialCall
	err   error
	// wrongDevice makes the dialer answer with a different device than asked.
	wrongDevice bool
}

func (d *fakeDialer) Session(_ context.Context, peer node.DeviceID, opt node.DialOpt) (node.Peer, *mux.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := opt
	cp.AccessSecret = append([]byte(nil), opt.AccessSecret...)
	d.calls = append(d.calls, dialCall{peer: peer, opt: cp})
	if d.err != nil {
		return node.Peer{}, nil, d.err
	}
	p := node.Peer{Device: peer}
	if opt.Card != nil {
		p.Node = node.NodeID(opt.Card.NodeID)
	}
	if d.wrongDevice {
		p.Device[0] ^= 0xff
	}
	return p, nil, nil
}

func (d *fakeDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func secretOf(url string) string { return url[strings.Index(url, "&s=")+3:] }

func TestContactLink(t *testing.T) {
	t.Parallel()
	r := newRig(t, cards.Visible)
	s := r.service(t, nil)

	l, err := s.ContactLink(t.Context())
	if err != nil {
		t.Fatalf("ContactLink: %v", err)
	}
	if !strings.HasPrefix(l.URL, "echofog://join?v=2&card=") || strings.Contains(l.URL, "&s=") {
		t.Fatalf("contact link %q", l.URL)
	}
	if !strings.HasPrefix(l.Web, "https://"+joinHost+"/join/#join?v=2&card=") {
		t.Fatalf("web form %q", l.Web)
	}
	if !l.Expires.After(testNow) {
		t.Fatalf("expires %v", l.Expires)
	}
	nodeID, deviceID := r.ids()
	for _, in := range []string{l.URL, l.Web} {
		p, err := s.Open(in)
		if err != nil {
			t.Fatalf("Open(%.40s): %v", in, err)
		}
		if p.Kind != links.KindContact || p.Node != nodeID || p.Device != deviceID || p.Card == nil {
			t.Fatalf("preview %+v", p)
		}
		if !p.NotAfter.Equal(l.Expires) {
			t.Fatalf("not after %v, link expires %v", p.NotAfter, l.Expires)
		}
	}
}

func TestContactLinkOfHiddenNode(t *testing.T) {
	t.Parallel()
	r := newRig(t, cards.Hidden)
	_, err := r.service(t, nil).ContactLink(t.Context())
	if !errors.Is(err, links.ErrNoContactLink) {
		t.Fatalf("hidden node contact link: %v", err)
	}
}

func TestInvite(t *testing.T) {
	t.Parallel()
	r := newRig(t, cards.Hidden)
	s := r.service(t, nil)

	inv, err := s.Invite(t.Context(), access.MintOptions{})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if !strings.Contains(inv.URL, "&s=") || !strings.HasPrefix(inv.Web, "https://"+joinHost+"/join/#join?v=2&card=") {
		t.Fatalf("invitation link %q / %q", inv.URL, inv.Web)
	}
	if inv.MaxEntries != 1 || !inv.Expires.After(testNow) {
		t.Fatalf("invitation %+v", inv)
	}
	nodeID, deviceID := r.ids()
	for _, in := range []string{inv.URL, inv.Web} {
		p, err := s.Open(in)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if p.Kind != links.KindInvitation || p.Node != nodeID || p.Device != deviceID {
			t.Fatalf("preview %+v", p)
		}
		// A preview is shown to the UI and logged: the access secret is not in it.
		if got := fmt.Sprintf("%+v %#v", p, p); strings.Contains(got, secretOf(inv.URL)) {
			t.Fatal("preview carries the access secret")
		}
	}
}

func TestInviteNeedsInviter(t *testing.T) {
	t.Parallel()
	r := newRig(t, cards.Visible)
	if _, err := r.service(t, nil).Invite(t.Context(), access.MintOptions{}); !errors.Is(err, links.ErrNoInviter) {
		t.Fatalf("Invite without an inviter: %v", err)
	}
}

func TestInvitationDoesNotPrintItsSecret(t *testing.T) {
	t.Parallel()
	r := newRig(t, cards.Hidden)
	inv, err := r.service(t, nil).Invite(t.Context(), access.MintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sec := secretOf(inv.URL)
	card := inv.URL[strings.Index(inv.URL, "card=")+5 : strings.Index(inv.URL, "&s=")]
	for _, got := range []string{
		inv.String(), fmt.Sprint(inv), fmt.Sprintf("%v", *inv), fmt.Sprintf("%+v", inv), fmt.Sprintf("%#v", inv), fmt.Sprintf("%s", inv),
	} {
		if strings.Contains(got, sec) || strings.Contains(got, card) || strings.Contains(got, "echofog://") {
			t.Fatalf("invitation prints its link: %q", got)
		}
	}
}

// TestSlashForm: v1 links were printed and pasted as echofog://join/?... (v1 dropJoinSlash); the
// v2 decoder takes that form, and the web form with the slash.
func TestSlashForm(t *testing.T) {
	t.Parallel()
	r := newRig(t, cards.Visible)
	s := r.service(t, nil)
	l, err := s.ContactLink(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slash := strings.Replace(l.URL, "echofog://join?", "echofog://join/?", 1)
	webSlash := strings.Replace(l.Web, "/join/#join?", "/join/#join/?", 1)
	for _, in := range []string{slash, webSlash, "  " + l.URL + "\n", "\t" + slash + " "} {
		if _, err := s.Open(in); err != nil {
			t.Errorf("Open(%.50q): %v", in, err)
		}
	}
}

func TestOpenRefusals(t *testing.T) {
	t.Parallel()
	pub := newRig(t, cards.Visible)
	hid := newRig(t, cards.Hidden)
	s := pub.service(t, nil)
	contact, err := s.ContactLink(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	inv, err := hid.service(t, nil).Invite(t.Context(), access.MintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	card64 := contact.URL[strings.Index(contact.URL, "card=")+5:]
	invCard64 := inv.URL[strings.Index(inv.URL, "card=")+5 : strings.Index(inv.URL, "&s=")]

	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", links.ErrNotLink},
		{"spaces", "   ", links.ErrNotLink},
		{"plain text", "hello", links.ErrNotLink},
		{"other host path", "https://example.org/other/#join?v=2&card=" + card64, links.ErrNotLink},
		{"other path", "echofog://relay/1.2.3.4:5/abc", links.ErrNotLink},
		{"no card", "echofog://join?v=2", links.ErrNotLink},
		{"bad base64", "echofog://join?v=2&card=!!!", links.ErrNotLink},
		{"other version", "echofog://join?v=3&card=" + card64, links.ErrOtherVersion},
		{"v1 link", "echofog://join?net=echofog&peer=abc", links.ErrNotLink},
		{"invitation card without a secret", "echofog://join?v=2&card=" + invCard64, links.ErrMismatch},
		{"contact card with a secret", "echofog://join?v=2&card=" + card64 + "&s=" + strings.Repeat("A", 22), links.ErrMismatch},
		{"wrong secret", "echofog://join?v=2&card=" + invCard64 + "&s=" + strings.Repeat("A", 22), links.ErrMismatch},
		{"short secret", "echofog://join?v=2&card=" + invCard64 + "&s=AAAA", links.ErrNotLink},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Open(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open: %v, want %v", err, tc.want)
			}
			if links.UserMessage(err) == "" {
				t.Fatalf("no user message for %v", err)
			}
		})
	}
}

func TestOpenExpired(t *testing.T) {
	t.Parallel()
	r := newRig(t, cards.Visible)
	clk := &fakeClock{now: testNow}
	s := r.serviceAt(t, nil, clk)
	l, err := s.ContactLink(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := links.Open(l.URL, l.Expires.Add(24*time.Hour)); !errors.Is(err, links.ErrExpired) {
		t.Fatalf("an expired card: %v", err)
	}
	if _, err := links.Open(l.URL, testNow); err != nil {
		t.Fatalf("a fresh card: %v", err)
	}
	clk.Advance(365 * 24 * time.Hour)
	if _, err := s.Open(l.URL); !errors.Is(err, links.ErrExpired) {
		t.Fatalf("Service.Open must use its clock: %v", err)
	}
}

// TestOpenIsBounded: a link comes from anywhere; a decompression bomb and an oversize link are
// refused without work proportional to their size (objects.md §10.1: stop at 4097 bytes).
func TestOpenIsBounded(t *testing.T) {
	t.Parallel()
	var z bytes.Buffer
	w, _ := flate.NewWriter(&z, flate.BestCompression)
	_, _ = w.Write(make([]byte, 8<<20))
	_ = w.Close()
	bomb := "echofog://join?v=2&card=" + base64.RawURLEncoding.EncodeToString(append([]byte{1}, z.Bytes()...))
	if _, err := links.Open(bomb, testNow); !errors.Is(err, links.ErrNotLink) {
		t.Fatalf("bomb: %v", err)
	}
	huge := "echofog://join?v=2&card=" + strings.Repeat("A", 1<<20)
	if _, err := links.Open(huge, testNow); !errors.Is(err, links.ErrNotLink) {
		t.Fatalf("1 MiB link: %v", err)
	}
	many := "echofog://join?v=2" + strings.Repeat("&x=1", 200000)
	if _, err := links.Open(many, testNow); !errors.Is(err, links.ErrNotLink) {
		t.Fatalf("many parameters: %v", err)
	}
}

func TestRedeemContactLink(t *testing.T) {
	t.Parallel()
	r := newRig(t, cards.Visible)
	d := &fakeDialer{}
	s := r.service(t, d)
	l, err := s.ContactLink(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Redeem(t.Context(), l.Web)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	nodeID, deviceID := r.ids()
	if p.Device != node.DeviceID(deviceID) || p.Node != node.NodeID(nodeID) {
		t.Fatalf("peer %+v", p)
	}
	if d.count() != 1 {
		t.Fatalf("%d dials", d.count())
	}
	c := d.calls[0]
	if c.peer != node.DeviceID(deviceID) || c.opt.Card == nil || c.opt.Card.DeviceID != deviceID || len(c.opt.AccessSecret) != 0 {
		t.Fatalf("dial %+v", c)
	}
}

func TestRedeemInvitation(t *testing.T) {
	t.Parallel()
	r := newRig(t, cards.Hidden)
	d := &fakeDialer{}
	s := r.service(t, d)
	inv, err := s.Invite(t.Context(), access.MintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redeem(t.Context(), inv.URL); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if d.count() != 1 || len(d.calls[0].opt.AccessSecret) != 16 {
		t.Fatalf("dial %+v", d.calls)
	}
	if pid := objects.PassID(d.calls[0].opt.AccessSecret); access.PassID(pid) != inv.PassID {
		t.Fatalf("dialed with the secret of pass %x, want %s", pid, inv.PassID)
	}
	if d.calls[0].opt.Card == nil || d.calls[0].opt.Card.Scope != objects.ScopeInvite {
		t.Fatalf("invitation card not passed: %+v", d.calls[0].opt.Card)
	}
}

func TestRedeemFailures(t *testing.T) {
	t.Parallel()
	pub := newRig(t, cards.Visible)
	hid := newRig(t, cards.Hidden)
	boom := errors.New("carrier: all attempts failed")

	t.Run("invitation not reachable reads as revoked", func(t *testing.T) {
		d := &fakeDialer{err: boom}
		s := hid.service(t, d)
		inv, err := s.Invite(t.Context(), access.MintOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// A hidden node closes silently on every failure (handshake.md §5), so a revoked pass,
		// a used-up one and an offline node look the same to the joiner.
		if _, err := s.Redeem(t.Context(), inv.URL); !errors.Is(err, links.ErrRevoked) {
			t.Fatalf("Redeem: %v", err)
		}
	})
	t.Run("route policy refusal is not revoked", func(t *testing.T) {
		for _, policy := range []error{node.ErrNoDomesticEntry, node.ErrRouteUnsupported} {
			d := &fakeDialer{err: fmt.Errorf("dial: %w", policy)}
			s := hid.service(t, d)
			inv, err := s.Invite(t.Context(), access.MintOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Redeem(t.Context(), inv.URL); !errors.Is(err, policy) || errors.Is(err, links.ErrRevoked) {
				t.Fatalf("invitation, %v: %v", policy, err)
			}
			cs := pub.service(t, &fakeDialer{err: policy})
			if _, err := cs.Redeem(t.Context(), mustContact(t, cs).URL); !errors.Is(err, policy) {
				t.Fatalf("contact, %v: %v", policy, err)
			}
		}
	})
	t.Run("contact link failure is not revoked", func(t *testing.T) {
		d := &fakeDialer{err: boom}
		s := pub.service(t, d)
		l := mustContact(t, s)
		_, err := s.Redeem(t.Context(), l.URL)
		if err == nil || errors.Is(err, links.ErrRevoked) || !errors.Is(err, boom) {
			t.Fatalf("Redeem: %v", err)
		}
	})
	t.Run("cancelled context is not revoked", func(t *testing.T) {
		d := &fakeDialer{err: context.Canceled}
		s := hid.service(t, d)
		inv := mustInvite(t, s)
		_, err := s.Redeem(t.Context(), inv.URL)
		if !errors.Is(err, context.Canceled) || errors.Is(err, links.ErrRevoked) {
			t.Fatalf("Redeem: %v", err)
		}
	})
	t.Run("bad link does not dial", func(t *testing.T) {
		d := &fakeDialer{}
		s := pub.service(t, d)
		if _, err := s.Redeem(t.Context(), "echofog://join?v=9&card=AAAA"); !errors.Is(err, links.ErrOtherVersion) {
			t.Fatalf("Redeem: %v", err)
		}
		if d.count() != 0 {
			t.Fatal("dialed on a bad link")
		}
	})
	t.Run("expired card does not dial", func(t *testing.T) {
		d := &fakeDialer{}
		clk := &fakeClock{now: testNow}
		s := pub.serviceAt(t, d, clk)
		l := mustContact(t, s)
		clk.Advance(365 * 24 * time.Hour)
		if _, err := s.Redeem(t.Context(), l.URL); !errors.Is(err, links.ErrExpired) {
			t.Fatalf("Redeem: %v", err)
		}
		if d.count() != 0 {
			t.Fatal("dialed an expired card")
		}
	})
	t.Run("no dialer", func(t *testing.T) {
		s := pub.service(t, nil)
		l := mustContact(t, s)
		if _, err := s.Redeem(t.Context(), l.URL); !errors.Is(err, links.ErrNoDialer) {
			t.Fatalf("Redeem: %v", err)
		}
	})
	t.Run("answer from another device", func(t *testing.T) {
		d := &fakeDialer{wrongDevice: true}
		s := pub.service(t, d)
		l := mustContact(t, s)
		if _, err := s.Redeem(t.Context(), l.URL); err == nil {
			t.Fatal("accepted a session with a device other than the card's")
		}
	})
}

func TestUserMessages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want string
	}{
		{links.ErrOtherVersion, "другой версией"},
		{links.ErrRevoked, "отозвана"},
		{links.ErrNotLink, "не ссылка EchoFog"},
		{fmt.Errorf("wrapped: %w", links.ErrNotLink), "не ссылка EchoFog"},
		{links.ErrExpired, "срок"},
		{errors.New("internal detail 10.0.0.1"), ""},
	}
	for _, tc := range cases {
		got := links.UserMessage(tc.err)
		if tc.want == "" {
			if strings.Contains(got, "10.0.0.1") {
				t.Errorf("internal detail leaks into the user message: %q", got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("UserMessage(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func mustContact(t *testing.T, s *links.Service) *links.Link {
	t.Helper()
	l, err := s.ContactLink(t.Context())
	if err != nil {
		t.Fatalf("ContactLink: %v", err)
	}
	return l
}

func mustInvite(t *testing.T, s *links.Service) *links.Invitation {
	t.Helper()
	inv, err := s.Invite(t.Context(), access.MintOptions{})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	return inv
}
