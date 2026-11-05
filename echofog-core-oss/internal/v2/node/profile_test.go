package node_test

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

// idlePaddingFeature is registries.md §5 feature 0x0003. The runtime does not define the full
// feature list; the application passes it in Config.Features.
const idlePaddingFeature uint32 = 0x0003

// ---- profiles (platforms.md §2)

// TestDefaultProfiles pins the default flag set of every platform profile.
func TestDefaultProfiles(t *testing.T) {
	cases := []struct {
		platform node.Platform
		want     node.Profile
	}{
		{
			platform: node.PlatformAndroid,
			want: node.Profile{
				Platform:   node.PlatformAndroid,
				LowPower:   false,
				Metered:    false,
				Background: node.BackgroundForeground,
				TunFD:      -1,
				MemCapMB:   128,
				UDP:        node.UDPRestricted,
			},
		},
		{
			platform: node.PlatformIOS,
			want: node.Profile{
				Platform:   node.PlatformIOS,
				LowPower:   false,
				Metered:    false,
				Background: node.BackgroundForeground,
				TunFD:      -1,
				MemCapMB:   40,
				UDP:        node.UDPRestricted,
			},
		},
		{
			platform: node.PlatformDesktop,
			want: node.Profile{
				Platform:   node.PlatformDesktop,
				Background: node.BackgroundForeground,
				TunFD:      -1,
				UDP:        node.UDPFull,
			},
		},
		{
			platform: node.PlatformServer,
			want: node.Profile{
				Platform:   node.PlatformServer,
				Roles:      []node.Role{node.RoleDHTServer, node.RoleMailbox, node.RoleRelay},
				Background: node.BackgroundForeground,
				TunFD:      -1,
				UDP:        node.UDPFull,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.platform), func(t *testing.T) {
			if got := node.DefaultProfile(tc.platform); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DefaultProfile(%s) = %+v, want %+v", tc.platform, got, tc.want)
			}
			if err := node.DefaultProfile(tc.platform).Validate(); err != nil {
				t.Fatalf("default profile does not validate: %v", err)
			}
		})
	}
}

// TestMobileDefaultsDoNotEnableSavings pins decisions.md T3.7b п.2: the profile does not silently
// switch on battery/network savings; platforms §4 sets low_power and metered dynamically, and an
// explicit platform setting survives WithDefaults.
func TestMobileDefaultsDoNotEnableSavings(t *testing.T) {
	for _, platform := range []node.Platform{node.PlatformAndroid, node.PlatformIOS} {
		def := node.DefaultProfile(platform)
		if def.LowPower || def.Metered {
			t.Fatalf("%s default enables savings (low_power=%v metered=%v)", platform, def.LowPower, def.Metered)
		}
		// The platform layer (battery saver / metered network) turns them on at runtime.
		app := def
		app.LowPower, app.Metered = true, true
		got := app.WithDefaults()
		if !got.LowPower || !got.Metered {
			t.Fatalf("%s: WithDefaults dropped the platform's explicit flags: %+v", platform, got)
		}
	}
}

// TestProfileDefaultsAndValidation: the zero value is completed to a safe desktop-like profile;
// unknown values are refused rather than silently treated as something else.
func TestProfileDefaultsAndValidation(t *testing.T) {
	got := node.Profile{}.WithDefaults()
	if got.Platform != node.PlatformDesktop {
		t.Fatalf("zero profile platform %q, want desktop", got.Platform)
	}
	if got.Background != node.BackgroundForeground || got.UDP != node.UDPFull {
		t.Fatalf("zero profile flags: background %q udp %q", got.Background, got.UDP)
	}
	if got.TunFD != -1 {
		t.Fatalf("zero profile tun fd %d, want -1 (no TUN)", got.TunFD)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("zero profile does not validate: %v", err)
	}

	mobile := node.Profile{Platform: node.PlatformAndroid}.WithDefaults()
	if mobile.MemCapMB != 128 || mobile.UDP != node.UDPRestricted {
		t.Fatalf("android defaults not applied: %+v", mobile)
	}

	bad := []struct {
		name    string
		profile node.Profile
	}{
		{"unknown platform", node.Profile{Platform: "amiga"}},
		{"unknown background", node.Profile{Platform: node.PlatformDesktop, Background: "napping"}},
		{"unknown udp", node.Profile{Platform: node.PlatformDesktop, UDP: "sometimes"}},
		{"unknown role", node.Profile{Platform: node.PlatformServer, Roles: []node.Role{"admin"}}},
	}
	for _, tc := range bad {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.profile.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", tc.profile)
			}
		})
	}

	// New refuses an invalid profile instead of starting with a half-applied one.
	if _, err := node.New(node.Config{Profile: node.Profile{Platform: "amiga"}}); err == nil ||
		!strings.Contains(err.Error(), "profile") {
		t.Fatalf("New accepted an invalid profile: %v", err)
	}
}

// TestProfileTunFD pins decisions.md T3.7b п.5: tun_fd is −1 (no TUN) or a real descriptor above
// the standard streams; 0/1/2 and every other negative value are configuration errors.
func TestProfileTunFD(t *testing.T) {
	base := node.DefaultProfile(node.PlatformDesktop)
	for _, fd := range []int{0, 1, 2, -2, -100} {
		p := base
		p.TunFD = fd
		if err := p.Validate(); err == nil {
			t.Fatalf("Validate accepted tun_fd %d", fd)
		}
	}
	for _, fd := range []int{-1, 3, 4, 1024} {
		p := base
		p.TunFD = fd
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate rejected tun_fd %d: %v", fd, err)
		}
	}
	// New refuses the same invalid values before it opens anything.
	for _, fd := range []int{1, 2, -2} {
		cfg := node.Config{Profile: base}
		cfg.Profile.TunFD = fd
		if _, err := node.New(cfg); !errors.Is(err, node.ErrConfig) || !strings.Contains(err.Error(), "tun_fd") {
			t.Fatalf("New with tun_fd %d: %v, want an ErrConfig mentioning tun_fd", fd, err)
		}
	}
}

func TestProfileHasRole(t *testing.T) {
	server := node.DefaultProfile(node.PlatformServer)
	for _, r := range []node.Role{node.RoleDHTServer, node.RoleMailbox, node.RoleRelay} {
		if !server.HasRole(r) {
			t.Fatalf("server profile does not have role %q", r)
		}
	}
	if server.HasRole(node.RoleExit) {
		t.Fatal("server profile offers the exit role by default")
	}
	for _, r := range []node.Role{node.RoleRelay, node.RoleExit, node.RoleMailbox, node.RoleDHTServer} {
		if node.DefaultProfile(node.PlatformAndroid).HasRole(r) {
			t.Fatalf("mobile profile offers role %q (platforms.md §2: empty on mobile)", r)
		}
	}
}

// TestRuntimeAppliesProfileDefaults: New completes and validates the profile, Profile() reports
// the effective values.
func TestRuntimeAppliesProfileDefaults(t *testing.T) {
	n := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) {
		c.Profile = node.Profile{Platform: node.PlatformIOS}
	})
	got := n.rt.Profile()
	if got.MemCapMB != 40 || got.UDP != node.UDPRestricted || got.Background != node.BackgroundForeground {
		t.Fatalf("effective profile %+v", got)
	}
}

// TestRuntimeCarriesEveryFlag: the flags that no runtime component acts on yet are still carried
// for the tasks that consume them (carriers, DHT, PEX, relay, mailbox, identity).
func TestRuntimeCarriesEveryFlag(t *testing.T) {
	protect := func(uintptr) error { return nil }
	addrs := func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.0.2.7")} }
	profile := node.DefaultProfile(node.PlatformDesktop)
	profile.LowPower = false
	profile.Metered = true
	profile.Roles = []node.Role{node.RoleRelay, node.RoleExit}
	profile.TunFD = 7
	profile.Protect = protect
	profile.LocalAddrs = addrs
	profile.PortMapping = true
	profile.STUN = true
	profile.MemCapMB = 64
	profile.UDP = node.UDPRestricted
	profile.SecretStore = fakeProtector{}

	n := newTestNodeFull(t, cards.Hidden, false, nil, nil, nil, func(c *node.Config) {
		c.Profile = profile
	})
	got := n.rt.Profile()
	if !got.Metered || !got.PortMapping || !got.STUN || got.MemCapMB != 64 || got.UDP != node.UDPRestricted {
		t.Fatalf("carried profile %+v", got)
	}
	if got.TunFD != 7 || got.Protect == nil || got.Protect(9) != nil {
		t.Fatalf("protect/tun fd not carried: %+v", got)
	}
	if got.LocalAddrs == nil || len(got.LocalAddrs()) != 1 {
		t.Fatal("local_addrs callback not carried")
	}
	if got.SecretStore == nil {
		t.Fatal("secret_store callback not carried")
	}
	if !got.HasRole(node.RoleRelay) || !got.HasRole(node.RoleExit) {
		t.Fatalf("roles not carried: %v", got.Roles)
	}
}

type fakeProtector struct{}

func (fakeProtector) Protect([]byte, []byte) ([]byte, error)   { return nil, nil }
func (fakeProtector) Unprotect([]byte, []byte) ([]byte, error) { return nil, nil }

// TestLocalAddrsFeedTheCarrierPlan: the app's local addresses are the ones D21 compares against
// the responder addresses (platforms.md §2, wire.md §1 rule 3).
func TestLocalAddrsFeedTheCarrierPlan(t *testing.T) {
	addr := netip.MustParseAddr("198.51.100.9")
	n := newTestNodeFull(t, cards.Hidden, false, nil, nil, nil, func(c *node.Config) {
		c.Profile.LocalAddrs = func() []netip.Addr { return []netip.Addr{addr} }
	})
	info := node.LocalInfoOf(n.rt)
	if len(info.Addrs) != 1 || info.Addrs[0] != addr {
		t.Fatalf("carrier plan addresses %v, want [%v]", info.Addrs, addr)
	}
}

// TestMemCapLimitsTheSessionTable: mem_cap_mb selects the mobile session limit (platforms.md §2).
func TestMemCapLimitsTheSessionTable(t *testing.T) {
	mobile := newTestNodeFull(t, cards.Hidden, false, nil, nil, nil, func(c *node.Config) {
		c.Profile = node.DefaultProfile(node.PlatformAndroid)
	})
	mobile.start(t)
	if got := node.SessionLimitOf(mobile.rt); got != 32 {
		t.Fatalf("mobile session limit %d, want 32", got)
	}

	desktop := newTestNode(t, cards.Hidden, false, nil, nil)
	desktop.start(t)
	if got := node.SessionLimitOf(desktop.rt); got <= 32 {
		t.Fatalf("desktop session limit %d, want more than 32", got)
	}
}

// ---- low_power

// TestRuntimeLowPowerSession: low_power reaches the mux (40–55 s keepalive, no standby paths) and
// makes the node stop offering idle-padding (platforms.md §2).
func TestRuntimeLowPowerSession(t *testing.T) {
	qMobile, qDesktop := newCarrierQueue(), newCarrierQueue()
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{qMobile}, nil, func(c *node.Config) {
		c.Features = []uint32{idlePaddingFeature}
	})
	a := newTestNodeFull(t, cards.Hidden, false, nil, nil, nil, func(c *node.Config) {
		c.Profile = node.DefaultProfile(node.PlatformAndroid)
		c.Features = []uint32{idlePaddingFeature}
	})
	b.start(t)
	a.start(t)

	sess, _ := dialTo(t, a, b, qMobile)
	if !sess.LowPower() {
		t.Fatal("low_power profile did not reach the mux session")
	}
	if sess.HasFeature(uint16(idlePaddingFeature)) {
		t.Fatal("a low_power node offered idle-padding (platforms.md §2)")
	}

	// The same application feature list on a desktop profile negotiates idle-padding.
	d := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{qDesktop}, nil, func(c *node.Config) {
		c.Features = []uint32{idlePaddingFeature}
	})
	c := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) {
		c.Features = []uint32{idlePaddingFeature}
	})
	d.start(t)
	c.start(t)
	normal, _ := dialTo(t, c, d, qDesktop)
	if normal.LowPower() {
		t.Fatal("desktop session unexpectedly low power")
	}
	if !normal.HasFeature(uint16(idlePaddingFeature)) {
		t.Fatal("desktop session did not negotiate idle-padding")
	}
}

// ---- udp

// TestRuntimeLocalAddrsRefresh: a network change reported by the app is applied to the carrier
// plan without a runtime restart (platforms.md §2; the network event itself is T3.3).
func TestRuntimeLocalAddrsRefresh(t *testing.T) {
	first := netip.MustParseAddr("198.51.100.9")
	second := netip.MustParseAddr("203.0.113.44")
	current := first
	n := newTestNodeFull(t, cards.Hidden, false, nil, nil, nil, func(c *node.Config) {
		c.Profile.LocalAddrs = func() []netip.Addr { return []netip.Addr{current} }
	})
	if got := node.LocalInfoOf(n.rt).Addrs; len(got) != 1 || got[0] != first {
		t.Fatalf("carrier plan addresses %v, want [%v]", got, first)
	}
	current = second
	n.rt.RefreshLocalAddrs()
	if got := node.LocalInfoOf(n.rt).Addrs; len(got) != 1 || got[0] != second {
		t.Fatalf("carrier plan addresses after refresh %v, want [%v]", got, second)
	}
}

// TestRuntimeUDPBlockedHasNoPlan: udp=blocked reaches the D21 plan; every UDP suite is removed and
// only the relay fallback remains (platforms.md §2, wire.md §1).
func TestRuntimeUDPBlockedHasNoPlan(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	a := newTestNodeFull(t, cards.Hidden, false, nil, nil, nil, func(c *node.Config) {
		c.Profile = node.DefaultProfile(node.PlatformAndroid)
		c.Profile.UDP = node.UDPBlocked
	})
	b.start(t)
	a.start(t)

	info := node.LocalInfoOf(a.rt)
	if !info.UDPBlocked {
		t.Fatalf("udp=blocked did not reach the carrier plan: %+v", info)
	}
	for _, at := range carrier.Plan(b.peerCard(t), info) {
		if at.Kind != carrier.AttemptRelay {
			t.Fatalf("udp=blocked plan dials suite %d (kind %v), want relay only", at.Suite, at.Kind)
		}
	}
}

// ---- background

// TestRuntimeBackgroundSuspendedClosesAllSessions: suspended sends GOAWAY to every session and
// refuses new outgoing sessions; the operator is told through the typed event.
func TestRuntimeBackgroundSuspendedClosesAllSessions(t *testing.T) {
	events := &captureEvents{}
	q1, q2 := newCarrierQueue(), newCarrierQueue()
	b1 := newTestNode(t, cards.Visible, true, []carrier.Listener{q1}, nil)
	b2 := newTestNode(t, cards.Visible, true, []carrier.Listener{q2}, nil)
	a := newTestNodeWith(t, cards.Hidden, false, nil, events, func(c *node.Config) {
		c.RetryAfter = 42
	})
	b1.start(t)
	b2.start(t)
	a.start(t)

	s1, _ := dialTo(t, a, b1, q1)
	s2, _ := dialTo(t, a, b2, q2)

	if err := a.rt.SetBackground(node.BackgroundSuspended); err != nil {
		t.Fatalf("SetBackground: %v", err)
	}
	waitClosed(t, s1)
	waitClosed(t, s2)
	for i, s := range []*mux.Session{s1, s2} {
		var ga *mux.GoAwayError
		if !errors.As(s.Err(), &ga) {
			t.Fatalf("session %d ended with %v, want GOAWAY", i, s.Err())
		}
		if ga.Code != frame.CodeGoingAway || !ga.Remote || ga.RetryAfter != 42 {
			t.Fatalf("session %d GOAWAY %+v, want going-away/remote/retry_after=42", i, ga.GoAway)
		}
	}
	if !events.has(node.EventBackground) {
		t.Fatal("the operator was not told about the background mode")
	}
	if e, ok := events.last(node.EventBackground); !ok || e.Background != node.BackgroundSuspended || e.Closed != 2 {
		t.Fatalf("background event payload %+v, want suspended with 2 closed sessions", e)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := a.rt.Session(ctx, b1.id.DeviceID, node.DialOpt{Card: b1.peerCard(t)})
	if !errors.Is(err, node.ErrBackground) {
		t.Fatalf("dial while suspended: %v, want ErrBackground", err)
	}
}

// TestRuntimeBackgroundLimitedKeepsMailboxRelay: limited keeps exactly one session and prefers a
// mailbox relay, then a relay, then the most recently active session (platforms.md §2).
func TestRuntimeBackgroundLimitedKeepsMailboxRelay(t *testing.T) {
	qMail, qPlain, qRelay := newCarrierQueue(), newCarrierQueue(), newCarrierQueue()
	bMail := newTestNodeFull(t, cards.Visible, true, []carrier.Listener{qMail}, nil, func(c *cards.Config) {
		c.Flags = objects.FlagMailbox
	}, nil)
	bPlain := newTestNode(t, cards.Visible, true, []carrier.Listener{qPlain}, nil)
	bRelay := newTestNodeFull(t, cards.Visible, true, []carrier.Listener{qRelay}, nil, func(c *cards.Config) {
		c.Flags = objects.FlagRelay
	}, nil)
	events := &captureEvents{}
	a := newTestNode(t, cards.Hidden, false, nil, events)
	bMail.start(t)
	bPlain.start(t)
	bRelay.start(t)
	a.start(t)

	mail, _ := dialTo(t, a, bMail, qMail)
	plain, _ := dialTo(t, a, bPlain, qPlain)
	relay, _ := dialTo(t, a, bRelay, qRelay)

	if err := a.rt.SetBackground(node.BackgroundLimited); err != nil {
		t.Fatalf("SetBackground: %v", err)
	}
	waitClosed(t, plain)
	waitClosed(t, relay)
	if closedWithinS(mail, time.Second) {
		t.Fatal("limited closed the mailbox relay session")
	}
	if e, ok := events.last(node.EventBackground); !ok || e.Background != node.BackgroundLimited || e.Closed != 2 {
		t.Fatalf("background event payload %+v, want limited with 2 closed sessions", e)
	}
}

// TestRuntimeBackgroundLimitedKeepsMostRecent covers the third level of the order: with no
// mailbox or relay flags the most recently active session survives (decisions.md T3.7b п.4).
func TestRuntimeBackgroundLimitedKeepsMostRecent(t *testing.T) {
	q1, q2, q3 := newCarrierQueue(), newCarrierQueue(), newCarrierQueue()
	b1 := newTestNode(t, cards.Visible, true, []carrier.Listener{q1}, nil)
	b2 := newTestNode(t, cards.Visible, true, []carrier.Listener{q2}, nil)
	b3 := newTestNode(t, cards.Visible, true, []carrier.Listener{q3}, nil)
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b1.start(t)
	b2.start(t)
	b3.start(t)
	a.start(t)

	s1, _ := dialTo(t, a, b1, q1)
	s2, _ := dialTo(t, a, b2, q2)
	s3, _ := dialTo(t, a, b3, q3)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s3.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if err := a.rt.SetBackground(node.BackgroundLimited); err != nil {
		t.Fatalf("SetBackground: %v", err)
	}
	waitClosed(t, s1)
	waitClosed(t, s2)
	if closedWithinS(s3, time.Second) {
		t.Fatal("limited closed the most recently active session")
	}
}

// ---- GREASE (D18)

// TestRuntimeGreaseTimer: every session gets a GREASE timer with random intervals.
func TestRuntimeGreaseTimer(t *testing.T) {
	q1, q2 := newCarrierQueue(), newCarrierQueue()
	b1 := newTestNode(t, cards.Visible, true, []carrier.Listener{q1}, nil)
	b2 := newTestNode(t, cards.Visible, true, []carrier.Listener{q2}, nil)
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) {
		c.GreaseInterval = func() time.Duration { return 15 * time.Millisecond }
	})
	b1.start(t)
	b2.start(t)
	a.start(t)

	s1, _ := dialTo(t, a, b1, q1)
	s2, _ := dialTo(t, a, b2, q2)
	for i, s := range []*mux.Session{s1, s2} {
		if !eventually(5*time.Second, func() bool { return s.Stats().GreaseSent >= 2 }) {
			t.Fatalf("session %d sent %d GREASE frames, want at least 2", i, s.Stats().GreaseSent)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s1.Ping(ctx); err != nil {
		t.Fatalf("the peer broke on GREASE frames: %v", err)
	}
}

// TestGreaseIntervalIsRandom: the default interval is inside the chosen bounds and is not a
// constant (D18: "at random intervals", AD13: injectable timers).
func TestGreaseIntervalIsRandom(t *testing.T) {
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 200; i++ {
		d := node.GreaseIntervalDefault()
		if d < 30*time.Second || d > 90*time.Second {
			t.Fatalf("GREASE interval %v outside [30 s, 90 s]", d)
		}
		seen[d] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatal("GREASE interval is a constant, want random intervals")
	}
}

// ---- typed events

// TestVisibilityChangeEvent: a card reissue that changes the visibility is reported once and the
// runtime's identity follows it (T3.7a risk 4).
func TestVisibilityChangeEvent(t *testing.T) {
	events := &captureEvents{}
	n := newTestNode(t, cards.Visible, true, nil, events)
	n.start(t)

	node.NoteVisibility(n.rt, cards.Hidden)
	node.NoteVisibility(n.rt, cards.Hidden)
	if got := n.rt.Self().Visibility; got != cards.Hidden {
		t.Fatalf("identity visibility %v, want hidden", got)
	}
	if got := events.count(node.EventVisibility); got != 1 {
		t.Fatalf("visibility events %d, want 1", got)
	}
	if e, ok := events.last(node.EventVisibility); !ok || e.Visibility != cards.Hidden {
		t.Fatalf("visibility event payload %+v, want hidden", e)
	}
}

// TestListenerStoppedEvent: an accept loop that stops on a local error tells the operator instead
// of failing silently (T3.7a risk 3).
func TestListenerStoppedEvent(t *testing.T) {
	events := &captureEvents{}
	n := newTestNode(t, cards.Hidden, false, nil, events)
	node.NoteListenerStopped(n.rt, errors.New("card material is broken"))
	e, ok := events.last(node.EventListenerStopped)
	if !ok {
		t.Fatal("listener stop was not reported")
	}
	if e.Err == nil {
		t.Fatal("listener stop event has no error")
	}
}

// TestMemoryProtectionEvent: secret.Observe is wired to the facade event (T4.1 report: T3.7b
// connects it).
func TestMemoryProtectionEvent(t *testing.T) {
	events := &captureEvents{}
	n := newTestNode(t, cards.Hidden, false, nil, events)

	var observe func(secret.Status)
	node.SetSecretObserver(func(fn func(secret.Status)) { observe = fn })
	defer node.SetSecretObserver(secret.Observe)

	n.start(t)
	if observe == nil {
		t.Fatal("the runtime did not subscribe to secret.Observe")
	}
	observe(secret.Status{Level: secret.LevelOrdinary, Err: errors.New("mlock refused")})
	e, ok := events.last(node.EventMemoryProtection)
	if !ok {
		t.Fatal("reduced memory protection was not reported")
	}
	if e.Err == nil {
		t.Fatal("memory protection event has no error")
	}
	if e.MemoryLevel != secret.LevelOrdinary {
		t.Fatalf("memory protection level %v, want ordinary", e.MemoryLevel)
	}
}

// ---- helpers

func dialTo(t *testing.T, a, b *testNode, queue *carrierQueue) (*mux.Session, node.Peer) {
	t.Helper()
	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: pair.Client,
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	return sess, p
}

func waitClosed(t *testing.T, s *mux.Session) {
	t.Helper()
	if !closedWithinS(s, 5*time.Second) {
		t.Fatal("session was not closed")
	}
}

func closedWithinS(s *mux.Session, d time.Duration) bool {
	select {
	case <-s.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
