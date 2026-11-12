package bootstrap_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

const (
	hexA = "0101010101010101010101010101010101010101010101010101010101010101"
	hexM = "0202020202020202020202020202020202020202020202020202020202020202"
)

var testNow = time.Unix(1_800_000_000, 0)

func TestParseAddressForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		kind bootstrap.Kind
		bad  bool
	}{
		{"bare noise3", "noise3://192.0.2.10:4430?mask=" + hexM, bootstrap.KindAddr, false},
		{"device noise3", "noise3://192.0.2.10:4430?mask=" + hexM + "&device=" + hexA, bootstrap.KindDevice, false},
		{"reality2 quic with sni", "reality2-quic://[2001:db8::1]:443?mask=" + hexM + "&sni=www.example.org", bootstrap.KindAddr, false},
		{"reality2 without sni", "reality2-quic://192.0.2.10:443?mask=" + hexM, 0, true},
		{"no mask", "noise3://192.0.2.10:4430", 0, true},
		{"short mask", "noise3://192.0.2.10:4430?mask=0202", 0, true},
		{"short device", "noise3://192.0.2.10:4430?mask=" + hexM + "&device=01", 0, true},
		{"ws2 is not a bare address", "ws2://192.0.2.10:443?mask=" + hexM + "&sni=x.example", 0, true},
		{"unknown suite", "quic9://192.0.2.10:4430?mask=" + hexM, 0, true},
		{"port zero", "noise3://192.0.2.10:0?mask=" + hexM, 0, true},
		{"no port", "noise3://192.0.2.10?mask=" + hexM, 0, true},
		{"plain host:port is not a list entry", "192.0.2.10:4430", 0, true},
		{"garbage", "hello", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			es, err := bootstrap.Parse(tc.in, testNow)
			if tc.bad {
				if err == nil {
					t.Fatalf("accepted: %+v", es)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(es) != 1 || es[0].Kind != tc.kind {
				t.Fatalf("entries %+v", es)
			}
			if tc.kind == bootstrap.KindDevice && es[0].Device[0] != 1 {
				t.Fatal("device id lost")
			}
		})
	}
}

func TestParseListRules(t *testing.T) {
	line := "noise3://192.0.2.10:4430?mask=" + hexM
	es, err := bootstrap.Parse("# comment\n\n"+line+"\r\n  "+line+"  \nnoise3://192.0.2.11:4430?mask="+hexM+"\n", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("want a duplicate dropped: %d entries", len(es))
	}
	if _, err := bootstrap.Parse(line+"\nbad line\n", testNow); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error does not name the line: %v", err)
	}
	var many strings.Builder
	for i := 0; i < 65; i++ {
		many.WriteString("noise3://192.0.2.10:" + strconv.Itoa(4000+i) + "?mask=" + hexM + "\n")
	}
	if _, err := bootstrap.Parse(many.String(), testNow); err == nil {
		t.Fatal("65 entries accepted")
	}
	if _, err := bootstrap.Parse(strings.Repeat("#", 70<<10), testNow); err == nil {
		t.Fatal("an oversized list accepted")
	}
}

// ---- Service over a fake host

type fakeHost struct {
	mu        sync.Mutex
	sessions  []node.DeviceID
	bootstrap []node.BootstrapTarget
	opts      []node.DialOpt
	result    *node.BootstrapResult
	err       error
}

func (h *fakeHost) Session(_ context.Context, peer node.DeviceID, opt node.DialOpt) (node.Peer, *mux.Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions = append(h.sessions, peer)
	h.opts = append(h.opts, opt)
	return node.Peer{Device: peer}, nil, h.err
}

func (h *fakeHost) DialBootstrap(_ context.Context, t node.BootstrapTarget, opt node.DialOpt) (*node.BootstrapResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bootstrap = append(h.bootstrap, t)
	h.opts = append(h.opts, opt)
	return h.result, h.err
}

func run(t *testing.T, h *fakeHost, list string) bootstrap.Report {
	t.Helper()
	es, err := bootstrap.Parse(list, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return bootstrap.New(h, es, func() time.Time { return testNow }).Run(context.Background())
}

func TestRunBareAddressDoesNotJoin(t *testing.T) {
	h := &fakeHost{result: &node.BootstrapResult{Card: fakeCard()}}
	rep := run(t, h, "noise3://192.0.2.10:4430?mask="+hexM)
	if len(h.bootstrap) != 1 || h.bootstrap[0].Expected != nil {
		t.Fatalf("bootstrap dials: %+v", h.bootstrap)
	}
	if len(h.sessions) != 0 {
		t.Fatal("a bare address was turned into a trusted session")
	}
	if rep.Joined != 0 || len(rep.Candidates) != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestRunDeviceEntryJoinsThroughVerifiedCard(t *testing.T) {
	h := &fakeHost{result: &node.BootstrapResult{Card: fakeCard(), Verified: true}}
	rep := run(t, h, "noise3://192.0.2.10:4430?mask="+hexM+"&device="+hexA)
	if len(h.bootstrap) != 1 || h.bootstrap[0].Expected == nil || h.bootstrap[0].Expected[0] != 1 {
		t.Fatalf("expected device not passed: %+v", h.bootstrap)
	}
	if len(h.sessions) != 1 || rep.Joined != 1 {
		t.Fatalf("sessions %d report %+v", len(h.sessions), rep)
	}
}

func TestRunUnverifiedResultForDeviceEntryIsNotUsed(t *testing.T) {
	h := &fakeHost{result: &node.BootstrapResult{Card: fakeCard(), Verified: false}}
	rep := run(t, h, "noise3://192.0.2.10:4430?mask="+hexM+"&device="+hexA)
	if len(h.sessions) != 0 || rep.Joined != 0 || rep.Failed != 1 {
		t.Fatalf("an unverified result joined: sessions %d, %+v", len(h.sessions), rep)
	}
}

func TestRunFailureIsCounted(t *testing.T) {
	h := &fakeHost{err: errors.New("down")}
	rep := run(t, h, "noise3://192.0.2.10:4430?mask="+hexM+"&device="+hexA)
	if rep.Failed != 1 || rep.Joined != 0 {
		t.Fatalf("report %+v", rep)
	}
}

// A user's entry is an explicit dial: the country policy must not cut it, and a refusal is logged
// by class, without the line or the address (V2-FIELD-1).
func TestUserEntryIsExplicitAndRefusalIsClassed(t *testing.T) {
	h := &fakeHost{err: node.ErrNoDomesticEntry}
	es, err := bootstrap.Parse("noise3://192.0.2.10:4430?mask="+hexM, testNow)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rep := bootstrap.New(h, es, func() time.Time { return testNow }).Log(log).Run(context.Background())
	if rep.Failed != 1 {
		t.Fatalf("report %+v", rep)
	}
	if len(h.opts) != 1 || !h.opts[0].Explicit {
		t.Fatalf("a user's entry must be explicit: %+v", h.opts)
	}
	text := buf.String()
	if !strings.Contains(text, "class=policy") {
		t.Fatalf("the refusal has no class: %s", text)
	}
	if strings.Contains(text, "192.0.2.10") || strings.Contains(text, hexM) {
		t.Fatalf("the log carries the entry: %s", text)
	}
}

func TestBuiltinEntryStaysUnderThePolicy(t *testing.T) {
	h := &fakeHost{err: node.ErrNoDomesticEntry}
	e := bootstrap.Entry{
		Kind: bootstrap.KindAddr, Addr: &pb.Addr{Suite: objects.SuiteNoise3, Host: "192.0.2.10", Port: 4430},
		Mask: keys.New(keys.X25519, make([]byte, 32)), Builtin: true, Index: 7,
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_ = bootstrap.New(h, []bootstrap.Entry{e}, func() time.Time { return testNow }).Log(log).Run(context.Background())
	if len(h.opts) != 1 || h.opts[0].Explicit {
		t.Fatalf("an official entry must follow the policy: %+v", h.opts)
	}
	if text := buf.String(); !strings.Contains(text, "builtin=7") || !strings.Contains(text, "class=policy") {
		t.Fatalf("the official entry's refusal is not classed by number: %s", text)
	}
}
