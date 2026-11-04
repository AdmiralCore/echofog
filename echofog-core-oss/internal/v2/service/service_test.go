package service

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

func TestBuiltinTableMatchesServicesMD(t *testing.T) {
	// services.md "Max" columns (= registries.md §4).
	want := map[uint64]map[uint64]int{
		DHT:     {0x40: 64, 0x41: 65536, 0x42: 64, 0x43: 65536, 0x44: 24576, 0x45: 32, 0x46: 32},
		PEX:     {0x40: 64, 0x41: 98304},
		Chat:    {0x40: 61504, 0x41: 32, 0x42: 16, 0x43: 8256},
		Mailbox: {0x40: 128, 0x41: 32, 0x42: 65664, 0x43: 48, 0x44: 160, 0x45: 1048576, 0x46: 2048, 0x47: 73728, 0x48: 32},
		Relay:   {0x40: 256, 0x41: 128, 0x42: 32, 0x43: 16, 0x44: 64, 0x45: 64, 0x46: 8320, 0x47: 32, 0x48: 32},
		Exit:    {0x40: 288, 0x41: 8, 0x42: 16384, 0x43: 16, 0x44: 32, 0x45: 65800, 0x46: 0, 0x47: 2048, 0x48: 512, 0x49: 64},
		Files:   {0x40: 4096, 0x41: 67584, 0x42: 32, 0x43: 32},
		Call:    {0x40: 65536},
	}
	for id, types := range want {
		sp, ok := Builtin(id)
		if !ok {
			t.Fatalf("service %d missing", id)
		}
		v, ok := sp.Versions[2]
		if !ok || len(sp.Versions) != 1 {
			t.Fatalf("%s: versions %v, want only 2", sp.Name, sp.Versions)
		}
		lim := v.Limiter()
		for typ, max := range types {
			if m, ok := lim(typ); !ok || m != max {
				t.Errorf("%s/2 type %#x: max %d %v, want %d", sp.Name, typ, m, ok, max)
			}
		}
		for typ := frame.ServiceLocalMin; typ < 0x60; typ++ {
			if _, listed := types[typ]; !listed {
				if _, ok := lim(typ); ok {
					t.Errorf("%s/2 type %#x is not in services.md", sp.Name, typ)
				}
			}
		}
	}
	for _, id := range []uint64{0, Names, Sync} {
		if _, ok := Builtin(id); ok {
			t.Errorf("reserved service %d has a definition", id)
		}
	}
	// 0-RTT safe operations (registries.md §3).
	if !EarlySafe(DHT, 2, 0x40) || !EarlySafe(DHT, 2, 0x42) || EarlySafe(DHT, 2, 0x44) || !EarlySafe(PEX, 2, 0x40) || EarlySafe(Chat, 2, 0x40) {
		t.Fatal("0-RTT safety table")
	}
}

func TestFeatureNegotiation(t *testing.T) {
	sup := []uint16{FeatureObservedAddr, FeaturePathMigration, FeaturePathMigration, FeatureCardPush}
	offered := OfferFeatures(sup, true)
	greases := 0
	for i, f := range offered {
		if IsGrease(f) {
			greases++
		}
		if i > 0 && offered[i-1] >= f {
			t.Fatalf("offer not ascending and unique: %#v", offered)
		}
	}
	if greases != 1 || len(offered) != 4 {
		t.Fatalf("offer %#v: want 3 features and one GREASE value", offered)
	}
	// The responder supports a GREASE value by mistake and an extra feature: neither is selected.
	sel := SelectFeatures(offered, []uint16{FeaturePathMigration, FeatureEarlyData, offered[slices.IndexFunc(offered, IsGrease)]})
	if !slices.Equal(sel, []uint16{FeaturePathMigration}) {
		t.Fatalf("selected %#v", sel)
	}
	if err := VerifyFeatures(offered, sel); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFeatures(offered, []uint16{FeatureEarlyData}); err == nil {
		t.Fatal("initiator accepted a feature it did not offer")
	}
	g := offered[slices.IndexFunc(offered, IsGrease)]
	if err := VerifyFeatures(offered, []uint16{g}); err == nil {
		t.Fatal("initiator accepted a selected GREASE value")
	}
	// GREASE values are drawn from all 15.
	seen := map[uint16]bool{}
	for i := 0; i < 2000; i++ {
		seen[RandomGrease()] = true
	}
	if len(seen) != 15 {
		t.Fatalf("%d distinct GREASE values", len(seen))
	}
}

func testRegistry(t testing.TB) *Registry {
	r := NewRegistry()
	r.Serve(Chat, 2, func(ctx context.Context, st *mux.Stream) {
		for {
			f, err := st.ReadFrame()
			if err != nil {
				return
			}
			if f.Type == 0x40 {
				st.WriteFrame(0x41, 0, []byte{0x08, 1})
			}
		}
	})
	r.Use(DHT, 2) // used as a client only
	return r
}

func TestServiceOfferAndSelect(t *testing.T) {
	r := testRegistry(t)
	offer := r.Offer(true)
	ids := []uint64{}
	greases := 0
	for _, o := range offer {
		ids = append(ids, o.ID)
		if IsGreaseService(o.ID) {
			greases++
			if !slices.Equal(o.Versions, []uint64{1}) {
				t.Fatalf("GREASE service versions %v, want [1]", o.Versions)
			}
		}
	}
	if !slices.IsSorted(ids) || greases != 1 || len(offer) != 3 {
		t.Fatalf("offer %+v", offer)
	}
	// The responder serves chat 2 and 3 and an unrelated service; it picks the highest common
	// version, ignores GREASE and services it does not serve.
	resp := NewRegistry()
	resp.Serve(Chat, 2, nil)
	resp.ServeSpec(Spec{ID: Chat, Name: "chat", Versions: map[uint64]Version{3: {}}}, 3, nil)
	resp.Serve(Files, 2, nil)
	choices := resp.Select(append(offer, Offer{ID: 0xf00d, Versions: []uint64{1}}))
	if len(choices) != 1 || choices[0] != (Choice{ID: Chat, Version: 2}) {
		t.Fatalf("choices %+v", choices)
	}
	if err := VerifyChoices(offer, choices); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChoices(offer, []Choice{{ID: Chat, Version: 3}}); err == nil {
		t.Fatal("initiator accepted a version it did not offer")
	}
	if err := VerifyChoices(offer, []Choice{{ID: Files, Version: 2}}); err == nil {
		t.Fatal("initiator accepted a service it did not offer")
	}
	if err := VerifyChoices(offer, []Choice{{ID: Chat, Version: 2}, {ID: Chat, Version: 2}}); err == nil {
		t.Fatal("initiator accepted two choices for one service")
	}
	// Highest common version when both offer several.
	both := NewRegistry()
	both.Serve(Chat, 2, nil)
	both.ServeSpec(Spec{ID: Chat, Name: "chat", Versions: map[uint64]Version{3: {}}}, 3, nil)
	if c := resp.Select(both.Offer(false)); len(c) != 1 || c[0].Version != 3 {
		t.Fatalf("%+v", c)
	}
}

// ---- sessions over a fake handshake

type keys struct {
	sid       [16]byte
	initiator bool
	secret, h []byte
	i2r, r2i  []byte
}

func (k *keys) SID() [16]byte                   { return k.sid }
func (k *keys) Initiator() bool                 { return k.initiator }
func (k *keys) SessionSecret() []byte           { return k.secret }
func (k *keys) TranscriptHash() []byte          { return k.h }
func (k *keys) TransportKeys() ([]byte, []byte) { return k.i2r, k.r2i }

type peer struct {
	reg      *Registry
	features []uint16
	abuse    atomic.Int32
}

type link struct {
	a, b       *mux.Session // a initiated
	polA, polB *Policy
}

// connect negotiates as a handshake would (services and features in HsInit/HsResp) and starts
// both sessions over a mux carrier.
func connect(t testing.TB, ini, resp *peer) *link {
	t.Helper()
	offer := ini.reg.Offer(true)
	choices := resp.reg.Select(offer)
	if err := VerifyChoices(offer, choices); err != nil {
		t.Fatal(err)
	}
	fo := OfferFeatures(ini.features, true)
	fs := SelectFeatures(fo, resp.features)
	if err := VerifyFeatures(fo, fs); err != nil {
		t.Fatal(err)
	}
	ik := &keys{initiator: true, secret: make([]byte, 32), h: make([]byte, 32), i2r: make([]byte, 32), r2i: make([]byte, 32)}
	for _, b := range [][]byte{ik.sid[:], ik.secret, ik.h, ik.i2r, ik.r2i} {
		rand.Read(b)
	}
	rk := *ik
	rk.initiator = false

	pa, pb := net.Pipe()
	ca := mux.NewConn(pa, mux.ConnConfig{Client: true})
	cb := mux.NewConn(pb, mux.ConnConfig{})
	ga, gb := mux.NewGuard(ca), mux.NewGuard(cb)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw0, _ := ca.OpenStream(ctx)
	raw0.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeHsHello, Payload: []byte{1, 0, 1, 0, 1, 0}}))
	_, pend, err := mux.NewBinder().Accept(ctx, gb, mux.PathDirect)
	if !errors.Is(err, mux.ErrNewSession) {
		t.Fatal(err)
	}
	l := &link{polA: ini.reg.Policy(choices), polB: resp.reg.Policy(choices)}
	l.a, err = mux.NewSession(mux.Config{Keys: ik, Features: fs, Policy: l.polA,
		OnAbuse: func(mux.Abuse) { ini.abuse.Add(1) }}, ga, raw0, mux.PathDirect)
	if err != nil {
		t.Fatal(err)
	}
	l.b, err = mux.NewSession(mux.Config{Keys: &rk, Features: fs, Policy: l.polB,
		OnAbuse: func(mux.Abuse) { resp.abuse.Add(1) }}, pend.Guard, pend.Stream, mux.PathDirect)
	if err != nil {
		t.Fatal(err)
	}
	sctx, scancel := context.WithCancel(context.Background())
	go Serve(sctx, l.a, l.polA)
	go Serve(sctx, l.b, l.polB)
	t.Cleanup(func() { scancel(); l.a.Close(); l.b.Close(); ca.Close(); cb.Close() })
	return l
}

func ctxT(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestUnknownServiceGetsUnsupported(t *testing.T) {
	ini := &peer{reg: testRegistry(t)}
	resp := &peer{reg: testRegistry(t)}
	l := connect(t, ini, resp)
	ctx := ctxT(t)
	check := func(svc, ver uint64, min, max uint64) {
		t.Helper()
		st, err := l.a.OpenStream(ctx, svc, ver)
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.ReadFrame()
		var ue *mux.UnsupportedError
		if !errors.As(err, &ue) || ue.Service != svc || ue.Min != min || ue.Max != max {
			t.Fatalf("OPEN %d/%d: %v, want UNSUPPORTED{%d, %d, %d}", svc, ver, err, svc, min, max)
		}
	}
	check(0xf00d, 1, 0, 0) // unknown service
	check(Names, 2, 0, 0)  // reserved service
	check(Chat, 3, 2, 2)   // known service, version not selected: chat/2 is served
	check(DHT, 2, 0, 0)    // the responder does not serve dht/2 (it only uses it)
	// Not abuse, and the session lives on.
	if ini.abuse.Load()+resp.abuse.Load() != 0 {
		t.Fatal("UNSUPPORTED counted as abuse")
	}
	st, err := Open(ctx, l.a, l.polA, Chat)
	if err != nil {
		t.Fatal(err)
	}
	st.WriteFrame(0x40, 0, []byte{0x08, 1})
	if f, err := st.ReadFrame(); err != nil || f.Type != 0x41 {
		t.Fatalf("%v", err)
	}
	if _, err := Open(ctx, l.a, l.polA, Files); !errors.Is(err, ErrNotSelected) {
		t.Fatalf("%v", err)
	}
}

func TestServiceFrameLimitEnforced(t *testing.T) {
	ini := &peer{reg: testRegistry(t)}
	resp := &peer{reg: testRegistry(t)}
	l := connect(t, ini, resp)
	st, _ := Open(ctxT(t), l.a, l.polA, Chat)
	st.WriteFrame(0x41, 0, make([]byte, 33)) // DELIVERED max 32
	_, err := st.ReadFrame()
	var ce *mux.CloseError
	if !errors.As(err, &ce) || ce.Code != frame.CodeFrameTooLarge {
		t.Fatalf("%v", err)
	}
	// An unknown chat/2 type without IGNORABLE: CLOSE{unknown-frame}, no abuse.
	st2, _ := Open(ctxT(t), l.a, l.polA, Chat)
	st2.WriteFrame(0x50, 0, []byte("?"))
	_, err = st2.ReadFrame()
	if !errors.As(err, &ce) || ce.Code != frame.CodeUnknownFrame {
		t.Fatalf("%v", err)
	}
	if resp.abuse.Load() != 1 {
		t.Fatalf("abuse %d, want 1 (only the oversized known frame)", resp.abuse.Load())
	}
}

func TestServeEndsWithSession(t *testing.T) {
	ini := &peer{reg: testRegistry(t)}
	resp := &peer{reg: testRegistry(t)}
	l := connect(t, ini, resp)
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), l.b, l.polB) }()
	l.b.Close()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, io.EOF) {
			t.Logf("serve ended: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the session closed")
	}
}
