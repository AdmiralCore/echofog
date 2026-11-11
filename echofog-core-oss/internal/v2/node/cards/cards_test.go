package cards

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

func TestPublicCardExcludesNoise3(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	cfg := baseConfig(t, m, newMapKV(), clk)
	noiseMask := newMask(t)
	realityMask := cfg.Listeners[0].Mask
	cfg.Listeners = append(cfg.Listeners, Listener{
		Suite: objects.SuiteNoise3,
		Host:  "203.0.113.10",
		Port:  443,
		Mask:  noiseMask,
	})

	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	issued, err := b.Issue(context.Background(), objects.ScopePublic)
	if err != nil {
		t.Fatalf("Issue(public): %v", err)
	}
	c := verifyBytes(t, issued.Envelope, clk.Now())

	if c.Scope != objects.ScopePublic {
		t.Fatalf("scope = %s", c.Scope)
	}
	for _, a := range c.Body.GetAddrs() {
		if a.GetSuite() == objects.SuiteNoise3 {
			t.Fatalf("public card carries a noise3 address")
		}
	}
	for _, k := range c.Body.GetMaskKeys() {
		if bytes.Equal(k.GetKey(), noiseMask) {
			t.Fatalf("public card carries the noise3 masking key")
		}
	}
	found := false
	for _, k := range c.Body.GetMaskKeys() {
		if bytes.Equal(k.GetKey(), realityMask) {
			found = true
		}
	}
	if !found {
		t.Fatalf("public card lost the REALITY masking key")
	}
}

func TestPublicCardNeedsCover(t *testing.T) {
	m := newMaterial(t)
	cfg := baseConfig(t, m, newMapKV(), newFakeClock())
	cfg.Listeners = []Listener{{
		Suite: objects.SuiteNoise3,
		Host:  "203.0.113.10",
		Port:  443,
		Mask:  newMask(t),
	}}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.Issue(context.Background(), objects.ScopePublic); !errors.Is(err, ErrNoPublicCard) {
		t.Fatalf("Issue(public) = %v, want ErrNoPublicCard", err)
	}
}

func TestHiddenAndAnonymousCards(t *testing.T) {
	for _, vis := range []Visibility{Hidden, Anonymous} {
		vis := vis
		t.Run(fmt.Sprint(vis), func(t *testing.T) {
			m := newMaterial(t)
			clk := newFakeClock()
			cfg := baseConfig(t, m, newMapKV(), clk)
			cfg.Visibility = vis
			cfg.Listeners = append(cfg.Listeners, Listener{
				Suite: objects.SuiteNoise3,
				Host:  "203.0.113.10",
				Port:  443,
				Mask:  newMask(t),
			})
			b, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := b.Issue(context.Background(), objects.ScopePublic); !errors.Is(err, ErrNoPublicCard) {
				t.Fatalf("Issue(public) = %v, want ErrNoPublicCard", err)
			}
			peer, err := b.Issue(context.Background(), objects.ScopePeer)
			if err != nil {
				t.Fatalf("Issue(peer): %v", err)
			}
			c := verifyBytes(t, peer.Envelope, clk.Now())
			if c.Body.GetFlags()&objects.FlagUnlisted == 0 {
				t.Fatalf("hidden peer card is not unlisted")
			}
			foundNoise := false
			for _, a := range c.Body.GetAddrs() {
				if a.GetSuite() == objects.SuiteNoise3 {
					foundNoise = true
				}
			}
			if !foundNoise {
				t.Fatalf("hidden peer card lost its noise3 address")
			}
			own, err := b.OwnCards(context.Background())
			if err != nil {
				t.Fatalf("OwnCards: %v", err)
			}
			if _, ok := own.Current[uint32(objects.ScopePublic)]; ok {
				t.Fatalf("hidden node has a current public card")
			}
			if _, ok := own.Current[uint32(objects.ScopePeer)]; !ok {
				t.Fatalf("hidden node has no current peer card")
			}
		})
	}
}

func TestVisiblePeerCardIsNotUnlisted(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	b, err := New(baseConfig(t, m, newMapKV(), clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	peer, err := b.Issue(context.Background(), objects.ScopePeer)
	if err != nil {
		t.Fatalf("Issue(peer): %v", err)
	}
	c := verifyBytes(t, peer.Envelope, clk.Now())
	if c.Body.GetFlags()&objects.FlagUnlisted != 0 {
		t.Fatalf("visible peer card is unlisted")
	}
	public, err := b.Issue(context.Background(), objects.ScopePublic)
	if err != nil {
		t.Fatalf("Issue(public): %v", err)
	}
	pc := verifyBytes(t, public.Envelope, clk.Now())
	if pc.Body.GetFlags()&(objects.FlagUnlisted|objects.FlagInviteScoped) != 0 {
		t.Fatalf("public card carries a private-scope flag")
	}
}

func TestInviteCard(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	cfg := baseConfig(t, m, newMapKV(), clk)
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	inv, err := b.IssueInvite(context.Background(), InviteOptions{InviteID: id})
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}
	if inv.Scope != objects.ScopeInvite {
		t.Fatalf("scope = %s", inv.Scope)
	}
	if inv.NotAfter != uint64(clk.Now().Unix())+uint64(objects.InviteCardLifetime) {
		t.Fatalf("invite not_after = %d", inv.NotAfter)
	}
	c := verifyBytes(t, inv.Envelope, clk.Now())
	if !bytes.Equal(c.Body.GetInviteId(), id) {
		t.Fatalf("invite_id = %x", c.Body.GetInviteId())
	}
	if c.Body.GetFlags()&(objects.FlagUnlisted|objects.FlagInviteScoped) != objects.FlagUnlisted|objects.FlagInviteScoped {
		t.Fatalf("invite flags = %#x", c.Body.GetFlags())
	}
	if _, err := b.IssueInvite(context.Background(), InviteOptions{InviteID: id[:7]}); err == nil {
		t.Fatalf("short invite_id accepted")
	}
	if _, err := b.Current(context.Background(), objects.ScopeInvite); !errors.Is(err, ErrInviteScope) {
		t.Fatalf("Current(invite) = %v, want ErrInviteScope", err)
	}
}

func TestSeqAndLifetimes(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	b, err := New(baseConfig(t, m, newMapKV(), clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	pub, err := b.Issue(ctx, objects.ScopePublic)
	if err != nil {
		t.Fatalf("Issue(public): %v", err)
	}
	if want := uint64(testNow.Unix()); pub.Seq != want {
		t.Fatalf("first seq = %d, want %d", pub.Seq, want)
	}
	if want := uint64(testNow.Unix()) + uint64(objects.CardMaxLifetime); pub.NotAfter != want {
		t.Fatalf("public not_after = %d, want %d", pub.NotAfter, want)
	}
	peer, err := b.Issue(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("Issue(peer): %v", err)
	}
	if peer.Seq != pub.Seq+1 {
		t.Fatalf("second seq = %d, want %d", peer.Seq, pub.Seq+1)
	}
	clk.Advance(10 * time.Second)
	next, err := b.Issue(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("Issue(peer): %v", err)
	}
	if next.Seq != uint64(clk.Now().Unix()) {
		t.Fatalf("seq after time jump = %d, want %d", next.Seq, clk.Now().Unix())
	}
	verifyBytes(t, next.Envelope, clk.Now())
}

func TestCurrentReusesAndReissuesOnEvent(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	cfg := baseConfig(t, m, newMapKV(), clk)
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	first, err := b.Current(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("Current(peer): %v", err)
	}
	again, err := b.Current(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("Current(peer): %v", err)
	}
	if first.Seq != again.Seq || !bytes.Equal(first.Envelope, again.Envelope) {
		t.Fatalf("Current reissued an unchanged card")
	}

	cfg.Release = "0.3.1"
	if err := b.Update(cfg); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reissued, err := b.Current(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("Current(peer): %v", err)
	}
	if reissued.Seq <= first.Seq {
		t.Fatalf("reissued seq = %d, want > %d", reissued.Seq, first.Seq)
	}
	c := verifyBytes(t, reissued.Envelope, clk.Now())
	if c.Body.GetRelease() != "0.3.1" {
		t.Fatalf("release = %q", c.Body.GetRelease())
	}

	// Republish when less than 2 days remain.
	clk.Advance(7*24*time.Hour - RepublishBefore + time.Second)
	near, err := b.Current(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("Current(peer): %v", err)
	}
	if near.Seq <= reissued.Seq {
		t.Fatalf("near-expiry seq = %d, want > %d", near.Seq, reissued.Seq)
	}
	verifyBytes(t, near.Envelope, clk.Now())
}

func TestMaxFieldsFitTheCardLimit(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	cfg := baseConfig(t, m, newMapKV(), clk)

	listeners := make([]Listener, 0, 16)
	maskKeys := make([][]byte, 8)
	for i := range maskKeys {
		maskKeys[i] = newMask(t)
	}
	for i := 0; i < 16; i++ {
		if i%2 == 0 {
			listeners = append(listeners, Listener{
				Suite: objects.SuiteReality2QUIC,
				Host:  fmt.Sprintf("203.0.113.%d", i+1),
				Port:  443,
				SNI:   "example.com",
				Mask:  maskKeys[i%8],
			})
		} else {
			listeners = append(listeners, Listener{
				Suite: objects.SuiteWS2,
				URL:   fmt.Sprintf("wss://node%d.example.com/ws", i),
				SNI:   "example.com",
				Mask:  maskKeys[i%8],
			})
		}
	}
	cfg.Listeners = listeners

	shop := "https://example.com/"
	shop += strings.Repeat("a", objects.MaxShopLink-len(shop))
	cfg.Shop = []string{shop, shop, shop}
	cfg.Release = strings.Repeat("1", 32)

	services := make([]*pb.ServiceOffer, 16)
	for i := range services {
		services[i] = &pb.ServiceOffer{Id: uint32(i + 1), Versions: []uint32{1}}
	}
	cfg.Services = services
	features := make([]uint32, 64)
	for i := range features {
		features[i] = uint32(1000 + i)
	}
	cfg.Features = features
	cfg.PrekeysHash = randomBytes(t, 32)
	prev, err := identityKeyPair(t)
	if err != nil {
		t.Fatalf("prev hs: %v", err)
	}
	cfg.PrevHs = &pb.HsKey{Key: prev, NotAfter: uint64(testNow.Add(24 * time.Hour).Unix())}
	cfg.Exit = exitOffer(&pb.ExitOffer{Price: 1000, Region: "DE", FreeKib: 10})

	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for _, scope := range []objects.Scope{objects.ScopePublic, objects.ScopePeer} {
		issued, err := b.Issue(ctx, scope)
		if err != nil {
			t.Fatalf("Issue(%s): %v", scope, err)
		}
		if len(issued.Envelope) > 4096 {
			t.Fatalf("%s envelope = %d bytes", scope, len(issued.Envelope))
		}
		c := verifyBytes(t, issued.Envelope, clk.Now())
		t.Logf("%s card: %d-byte envelope, %d addresses, %d masking keys",
			scope, len(issued.Envelope), len(c.Body.GetAddrs()), len(c.Body.GetMaskKeys()))
	}
}

func TestTooLargeCardIsRejected(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	cfg := baseConfig(t, m, newMapKV(), clk)
	maskKeys := make([][]byte, 8)
	for i := range maskKeys {
		maskKeys[i] = newMask(t)
	}
	long := "wss://example.com/" + strings.Repeat("a", 512-len("wss://example.com/"))
	listeners := make([]Listener, 0, 16)
	for i := 0; i < 16; i++ {
		listeners = append(listeners, Listener{
			Suite: objects.SuiteWS2,
			URL:   long,
			SNI:   strings.Repeat("s", 253),
			Mask:  maskKeys[i%8],
		})
	}
	cfg.Listeners = listeners
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Issue(context.Background(), objects.ScopePeer)
	if !errors.Is(err, objects.ErrTooLarge) {
		t.Fatalf("Issue = %v, want objects.ErrTooLarge", err)
	}
}

func TestShopAndReleaseValidation(t *testing.T) {
	m := newMaterial(t)
	cases := []struct {
		name string
		cfg  func(*Config)
	}{
		{"bad scheme", func(c *Config) { c.Shop = []string{"http://example.com"} }},
		{"four links", func(c *Config) {
			c.Shop = []string{"https://a.example", "https://b.example", "https://c.example", "https://d.example"}
		}},
		{"control character", func(c *Config) { c.Shop = []string{"https://example.com/\x01"} }},
		{"long release", func(c *Config) { c.Release = strings.Repeat("x", 33) }},
		{"bad exit region", func(c *Config) { c.Exit = exitOffer(&pb.ExitOffer{Region: "de"}) }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t, m, newMapKV(), newFakeClock())
			tc.cfg(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New accepted an invalid configuration")
			}
		})
	}

	cfg := baseConfig(t, m, newMapKV(), newFakeClock())
	cfg.Shop = []string{"https://example.com/shop", "echofog://contact?node=abc"}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	issued, err := b.Issue(context.Background(), objects.ScopePeer)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c := verifyBytes(t, issued.Envelope, testNow)
	if len(c.Body.GetShop()) != 2 {
		t.Fatalf("shop = %v", c.Body.GetShop())
	}
}

func TestListenerLimits(t *testing.T) {
	m := newMaterial(t)
	mask := newMask(t)
	cases := []struct {
		name      string
		listeners func() []Listener
	}{
		{"no masking key", func() []Listener {
			return []Listener{{Suite: objects.SuiteReality2QUIC, Host: "203.0.113.1", Port: 443}}
		}},
		{"ws2 without url", func() []Listener {
			return []Listener{{Suite: objects.SuiteWS2, Mask: mask}}
		}},
		{"too many listeners", func() []Listener {
			out := make([]Listener, 17)
			for i := range out {
				out[i] = Listener{Suite: objects.SuiteReality2QUIC, Host: "203.0.113.1", Port: 443, Mask: mask}
			}
			return out
		}},
		{"too many masking keys", func() []Listener {
			out := make([]Listener, 9)
			for i := range out {
				out[i] = Listener{Suite: objects.SuiteReality2QUIC, Host: "203.0.113.1", Port: 443, Mask: newMask(t)}
			}
			return out
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t, m, newMapKV(), newFakeClock())
			cfg.Listeners = tc.listeners()
			if _, err := New(cfg); err == nil {
				t.Fatalf("New accepted invalid listeners")
			}
		})
	}
}

func TestUpdateKeepsThePreviousConfigOnError(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	cfg := baseConfig(t, m, newMapKV(), clk)
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := b.Current(ctx, objects.ScopePeer); err != nil {
		t.Fatalf("Current: %v", err)
	}
	bad := cfg
	bad.Shop = []string{"http://example.com"}
	if err := b.Update(bad); err == nil {
		t.Fatalf("Update accepted an invalid configuration")
	}
	cur, err := b.Current(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("Current after failed Update: %v", err)
	}
	c := verifyBytes(t, cur.Envelope, clk.Now())
	if c.Body.GetRelease() != cfg.Release {
		t.Fatalf("failed Update changed the material")
	}
}

func TestInviteExtraListeners(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	b, err := New(baseConfig(t, m, newMapKV(), clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	extra := Listener{Suite: objects.SuiteNoise3, Host: "203.0.113.20", Port: 444, Mask: newMask(t)}
	inv, err := b.IssueInvite(ctx, InviteOptions{InviteID: randomBytes(t, 8), Listeners: []Listener{extra}})
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}
	c := verifyBytes(t, inv.Envelope, clk.Now())
	found := false
	for _, a := range c.Body.GetAddrs() {
		if a.GetSuite() == objects.SuiteNoise3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("invitation card lost its own noise3 listener")
	}

	if _, err := b.IssueInvite(ctx, InviteOptions{
		InviteID:  randomBytes(t, 8),
		Listeners: make([]Listener, maxAddrs+1),
	}); err == nil {
		t.Fatalf("IssueInvite accepted too many listeners")
	}
}

func TestInvalidServiceDoesNotPanic(t *testing.T) {
	m := newMaterial(t)
	cfg := baseConfig(t, m, newMapKV(), newFakeClock())
	cfg.Services = []*pb.ServiceOffer{nil}
	if _, err := New(cfg); err == nil {
		t.Fatalf("New accepted a nil service")
	}
	if _, err := New(baseConfig(t, m, newMapKV(), newFakeClock())); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestExitOfferSetsTheFlag(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	cfg := baseConfig(t, m, newMapKV(), clk)
	cfg.Exit = exitOffer(&pb.ExitOffer{Price: 42, Region: "NL"})
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	issued, err := b.Issue(context.Background(), objects.ScopePublic)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c := verifyBytes(t, issued.Envelope, clk.Now())
	if c.Body.GetFlags()&objects.FlagExit == 0 || c.Body.GetExit() == nil {
		t.Fatalf("live exit offer is not marked")
	}

	cfg.Exit = nil
	b2, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	issued, err = b2.Issue(context.Background(), objects.ScopePublic)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c = verifyBytes(t, issued.Envelope, clk.Now())
	if c.Body.GetFlags()&objects.FlagExit != 0 || c.Body.GetExit() != nil {
		t.Fatalf("exit offer without a source")
	}
}

func TestDeviceListIsRequired(t *testing.T) {
	m := newMaterial(t)
	cfg := baseConfig(t, m, newMapKV(), newFakeClock())
	cfg.Devices = nil
	if _, err := New(cfg); err == nil {
		t.Fatalf("New accepted a card without device list")
	}
	cfg = baseConfig(t, m, newMapKV(), newFakeClock())
	cfg.Account = m.device.Public() // not an account key
	if _, err := New(cfg); err == nil {
		t.Fatalf("New accepted a non-account key")
	}
}

func TestRestartRemembersKnownCardSeq(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	kv := newMapKV()
	cfg := baseConfig(t, m, kv, clk)
	ctx := context.Background()

	b1, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := b1.Issue(ctx, objects.ScopePublic)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	peer, err := b1.Issue(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("Issue(peer): %v", err)
	}
	invite, err := b1.IssueInvite(ctx, InviteOptions{InviteID: []byte{1, 2, 3, 4, 5, 6, 7, 8}})
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}

	b2, err := New(cfg)
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	own, err := b2.OwnCards(ctx)
	if err != nil {
		t.Fatalf("OwnCards: %v", err)
	}
	cur, ok := own.Current[uint32(objects.ScopePublic)]
	if !ok {
		t.Fatalf("restart lost the current public card")
	}
	if cur.Seq != first.Seq || !bytes.Equal(cur.Envelope, first.Envelope) {
		t.Fatalf("restart changed the current public card")
	}
	peerCur, ok := own.Current[uint32(objects.ScopePeer)]
	if !ok || peerCur.Seq != peer.Seq || !bytes.Equal(peerCur.Envelope, peer.Envelope) {
		t.Fatalf("restart changed the current peer card")
	}
	rec, ok := own.Issued[first.Seq]
	if !ok || rec.Scope != uint32(objects.ScopePublic) {
		t.Fatalf("restart forgot the issued seq %d: %+v", first.Seq, own.Issued)
	}
	if rec, ok := own.Issued[invite.Seq]; !ok || rec.Scope != uint32(objects.ScopeInvite) {
		t.Fatalf("restart forgot the issued invitation seq: %+v", own.Issued)
	}

	cfg.Release = "0.3.1"
	if err := b2.Update(cfg); err != nil {
		t.Fatalf("Update: %v", err)
	}
	second, err := b2.Current(ctx, objects.ScopePublic)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if second.Seq <= first.Seq {
		t.Fatalf("reissued seq = %d, want > %d", second.Seq, first.Seq)
	}
	own, err = b2.OwnCards(ctx)
	if err != nil {
		t.Fatalf("OwnCards: %v", err)
	}
	if _, ok := own.Issued[first.Seq]; !ok {
		t.Fatalf("old seq forgotten after reissue")
	}
	if _, ok := own.Issued[second.Seq]; !ok {
		t.Fatalf("new seq not recorded")
	}
}

func TestIssuedMemoryIsPruned(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	cfg := baseConfig(t, m, newMapKV(), clk)
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	first, err := b.Issue(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// The device list lives 30 days too; refresh it before the check so the card material stays
	// valid and the test exercises only the issued memory.
	clk.Advance(31 * 24 * time.Hour)
	devices, err := m.account.SealSingleDeviceList(m.device, 2, clk.Now())
	if err != nil {
		t.Fatalf("device list: %v", err)
	}
	cfg.Devices = devices
	if err := b.Update(cfg); err != nil {
		t.Fatalf("Update: %v", err)
	}
	own, err := b.OwnCards(ctx)
	if err != nil {
		t.Fatalf("OwnCards: %v", err)
	}
	if _, ok := own.Issued[first.Seq]; ok {
		t.Fatalf("issued seq older than 30 days is still remembered")
	}
}

func TestKVFailureDoesNotReuseSeq(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	kv := newMapKV()
	b, err := New(baseConfig(t, m, kv, clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	first, err := b.Issue(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	kv.putErr = errors.New("disk full")
	second, err := b.Issue(ctx, objects.ScopePeer)
	if err == nil {
		t.Fatalf("Issue ignored a KV failure")
	}
	if second == nil || second.Seq <= first.Seq {
		t.Fatalf("seq did not advance after a failed write")
	}
}

func TestCorruptStateIsIgnored(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	kv := newMapKV()
	kv.set(seqKey(), []byte("garbage"))
	kv.set(issuedKey(), []byte{9, 9, 9})
	kv.set(currentKey(objects.ScopePublic), []byte{1, 2, 3})
	b, err := New(baseConfig(t, m, kv, clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	issued, err := b.Issue(context.Background(), objects.ScopePublic)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	verifyBytes(t, issued.Envelope, clk.Now())
}

func TestConcurrentCurrent(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	b, err := New(baseConfig(t, m, newMapKV(), clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			c, err := b.Current(ctx, objects.ScopePeer)
			if err == nil {
				_, err = objects.VerifyCard(c.Envelope, clk.Now())
			}
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Current: %v", err)
		}
	}
}

type fixedExit struct{ offer *pb.ExitOffer }

func (f fixedExit) ExitOffer() *pb.ExitOffer { return f.offer }

func exitOffer(o *pb.ExitOffer) ExitSource { return fixedExit{offer: o} }

// TestReplaceMaskMovesContactsKey covers the contacts-key rotation (T3.6b): the builder must
// switch its listener to the new key and reissue the peer card so a revocation does not leave
// contacts with a dead address.
func TestReplaceMaskMovesContactsKey(t *testing.T) {
	m := newMaterial(t)
	oldMask, nextMask := newMask(t), newMask(t)
	cfg := baseConfig(t, m, newMapKV(), newFakeClock())
	cfg.Listeners[0].Mask = oldMask
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	before, err := b.Current(t.Context(), objects.ScopePeer)
	if err != nil {
		t.Fatalf("peer card before: %v", err)
	}
	n, err := b.ReplaceMask(oldMask, nextMask)
	if err != nil {
		t.Fatalf("ReplaceMask: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d listeners changed, want 1", n)
	}
	after, err := b.Current(t.Context(), objects.ScopePeer)
	if err != nil {
		t.Fatalf("peer card after: %v", err)
	}
	if string(before.Envelope) == string(after.Envelope) {
		t.Fatal("the peer card was not reissued")
	}
	card := verifyBytes(t, after.Envelope, b.clk.Now())
	found := false
	for _, k := range card.Body.GetMaskKeys() {
		if string(k.GetKey()) == string(nextMask) {
			found = true
		}
		if string(k.GetKey()) == string(oldMask) {
			t.Fatal("the peer card still carries the old contacts key")
		}
	}
	if !found {
		t.Fatal("the peer card does not carry the new contacts key")
	}

	// The old key is gone after the replacement, so a second call changes nothing.
	if n, err := b.ReplaceMask(oldMask, nextMask); err != nil || n != 0 {
		t.Fatalf("second ReplaceMask = %d, %v; want 0, nil", n, err)
	}
}

// T10.9: a desktop turns its exit on and off while it runs; the next card carries the flag, or not.
func TestSetExitTurnsTheOfferOnAndOff(t *testing.T) {
	m := newMaterial(t)
	clk := newFakeClock()
	b, err := New(baseConfig(t, m, newMapKV(), clk))
	if err != nil {
		t.Fatal(err)
	}
	flag := func() (bool, uint64) {
		t.Helper()
		issued, err := b.Current(context.Background(), objects.ScopePeer)
		if err != nil {
			t.Fatal(err)
		}
		c := verifyBytes(t, issued.Envelope, clk.Now())
		return c.Body.GetFlags()&objects.FlagExit != 0 && c.Body.GetExit() != nil, issued.Seq
	}
	if on, _ := flag(); on {
		t.Fatal("a node without an exit source carries the flag")
	}
	if err := b.SetExit(exitOffer(&pb.ExitOffer{Region: "NL"})); err != nil {
		t.Fatal(err)
	}
	on, seq1 := flag()
	if !on {
		t.Fatal("the card does not carry the exit after SetExit")
	}
	if err := b.SetExit(nil); err != nil {
		t.Fatal(err)
	}
	off, seq2 := flag()
	if off {
		t.Fatal("the card still carries the exit after SetExit(nil)")
	}
	if seq2 <= seq1 {
		t.Fatalf("seq %d then %d: a changed card needs a higher seq", seq1, seq2)
	}
	if err := b.SetExit(exitOffer(&pb.ExitOffer{Region: "de"})); err == nil {
		t.Fatal("an invalid offer was accepted")
	}
	if off, _ := flag(); off {
		t.Fatal("a refused offer changed the card")
	}
}
