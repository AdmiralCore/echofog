package exit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

var allReasons = []Reason{ReasonNoCamouflage, ReasonUnreachable, ReasonBusy, ReasonPolicy, ReasonRateLimited, ReasonNoPath}

func TestReasonTexts(t *testing.T) {
	codes := map[string]bool{}
	texts := map[string]bool{}
	for _, r := range allReasons {
		code, msg := r.String(), r.Message()
		if code == "" || msg == "" {
			t.Fatalf("reason %d has no code or text", r)
		}
		if codes[code] || texts[msg] {
			t.Fatalf("reason %d repeats a code or text: %q %q", r, code, msg)
		}
		codes[code], texts[msg] = true, true
		cyr := false
		for _, c := range msg {
			cyr = cyr || unicode.Is(unicode.Cyrillic, c)
		}
		if !cyr {
			t.Errorf("text of %s is not Russian: %q", code, msg)
		}
		err := &Refusal{Reason: r, cause: errors.New("secret 203.0.113.9:443")}
		if strings.Contains(err.Error(), "203.0.113.9") || err.Error() == "" {
			t.Errorf("Refusal text of %s: %q", code, err.Error())
		}
		if got := ReasonOf(err); got != r {
			t.Errorf("ReasonOf(Refusal %s) = %s", code, got)
		}
		if got := ReasonOf(fmt.Errorf("wrapped: %w", err)); got != r {
			t.Errorf("ReasonOf(wrapped %s) = %s", code, got)
		}
	}
}

func TestReasonOfErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Reason
	}{
		{"nil", nil, ReasonUnknown},
		{"foreign error", errors.New("boom"), ReasonUnknown},
		{"core busy", core.ErrExitBusy, ReasonBusy},
		{"core denied", core.ErrExitDenied, ReasonPolicy},
		{"core disabled", core.ErrExitDisabled, ReasonPolicy},
		{"core dial failed", core.ErrExitDialFailed, ReasonUnreachable},
		{"close policy", &mux.CloseError{Code: CodePolicy, Remote: true}, ReasonPolicy},
		{"wrapped close policy", fmt.Errorf("x: %w", &mux.CloseError{Code: CodePolicy}), ReasonPolicy},
		{"payment is T10.3", core.ErrPaymentRequired, ReasonUnknown},
		{"close payment is T10.3", &mux.CloseError{Code: CodePaymentRequired}, ReasonUnknown},
	}
	for _, tc := range tests {
		if got := ReasonOf(tc.err); got != tc.want {
			t.Errorf("%s: ReasonOf = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestCardHasCamouflage(t *testing.T) {
	mk := func(suites ...uint32) *objects.Card {
		body := &pb.NodeCard{}
		for _, s := range suites {
			body.Addrs = append(body.Addrs, &pb.Addr{Suite: s, Host: "198.51.100.7", Port: 443, Mask: 1})
		}
		return &objects.Card{Body: body}
	}
	for name, tc := range map[string]struct {
		card *objects.Card
		want bool
	}{
		"nil":             {nil, false},
		"no addresses":    {mk(), false},
		"noise3 only":     {mk(objects.SuiteNoise3), false},
		"reality2 quic":   {mk(objects.SuiteNoise3, objects.SuiteReality2QUIC), true},
		"reality2 tcp":    {mk(objects.SuiteReality2TCP), true},
		"ws2":             {mk(objects.SuiteWS2), true},
		"unknown suite 9": {mk(9), false},
	} {
		if got := CardHasCamouflage(tc.card); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func dialReason(cr *clientRig, target string) (Reason, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := cr.cl.DialExitStream(ctx, target)
	if err == nil {
		defer conn.Close()
		_, err = conn.Read(make([]byte, 1))
	}
	return ReasonOf(err), err
}

// Each reason the server can give reaches the caller as its own Reason.
func TestClientRefusalReasons(t *testing.T) {
	t.Run("unreachable: closed port", func(t *testing.T) {
		cr := newClientRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}})
		if r, err := dialReason(cr, "127.0.0.1:1"); r != ReasonUnreachable {
			t.Fatalf("reason %s (%v)", r, err)
		}
	})
	t.Run("policy: private target", func(t *testing.T) {
		cr := newClientRig(t, rigConfig{})
		if r, err := dialReason(cr, "127.0.0.1:9"); r != ReasonPolicy {
			t.Fatalf("reason %s (%v)", r, err)
		}
	})
	t.Run("policy: blocked port", func(t *testing.T) {
		cr := newClientRig(t, rigConfig{})
		if r, err := dialReason(cr, "1.1.1.1:25"); r != ReasonPolicy {
			t.Fatalf("reason %s (%v)", r, err)
		}
	})
	t.Run("rate limited", func(t *testing.T) {
		cr := newClientRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true, Limits: Limits{TCPPerSecond: 1}}})
		_, _ = dialReason(cr, "127.0.0.1:1")
		r, err := dialReason(cr, "127.0.0.1:2")
		if r != ReasonRateLimited {
			t.Fatalf("reason %s (%v)", r, err)
		}
	})
	t.Run("busy: UDP sessions used up", func(t *testing.T) {
		cr := newClientRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true, Limits: Limits{UDPSessions: 1}}})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		u, err := cr.cl.DialExitUDP(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer u.Close()
		_, err = cr.cl.DialExitUDP(ctx)
		if r := ReasonOf(err); r != ReasonBusy {
			t.Fatalf("reason %s (%v)", r, err)
		}
	})
}

func failingOpen(err error) ClientConfig {
	return ClientConfig{OpenStream: func(context.Context) (*mux.Stream, error) { return nil, err }}
}

func TestClientNoPathAndNoCamouflage(t *testing.T) {
	boom := errors.New("carrier: dial 203.0.113.9:443 failed")
	for _, tc := range []struct {
		name  string
		cover bool
		want  Reason
	}{
		{"no session, exit with a cover", false, ReasonNoPath},
		{"no session, exit without a cover", true, ReasonNoCamouflage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := failingOpen(boom)
			cfg.NoCamouflage = tc.cover
			cl, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := cl.DialExitStream(ctx, "192.0.2.1:80")
			if err == nil {
				defer conn.Close()
				_, err = conn.Read(make([]byte, 1))
			}
			if got := ReasonOf(err); got != tc.want {
				t.Fatalf("stream: reason %s (%v)", got, err)
			}
			if strings.Contains(err.Error(), "203.0.113.9") || strings.Contains(err.Error(), "192.0.2.1") {
				t.Fatalf("error text leaks an address: %q", err)
			}
			_, err = cl.DialExitUDP(ctx)
			if got := ReasonOf(err); got != tc.want {
				t.Fatalf("udp: reason %s (%v)", got, err)
			}
		})
	}
}

// AD20: the caller learns the reason, never why. "Closed", "reset" and "DNS failure" are one
// reason with one text, one after another and when many dials run at once.
func TestClientUnreachableIsOneReason(t *testing.T) {
	cr := newClientRig(t, rigConfig{ServerConfig: ServerConfig{
		AllowPrivateTargets: true,
		Limits:              Limits{TCPPerSecond: 1000, TCPTargetsPerMin: 1000},
		Resolver: func(_ context.Context, host string) ([]net.IP, error) {
			if host == "nx.test" {
				return nil, errors.New("no such host")
			}
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		},
	}})
	targets := []string{"127.0.0.1:1", "127.0.0.1:2", "nx.test:80", "closed.test:80"}
	var want string
	for _, tg := range targets {
		r, err := dialReason(cr, tg)
		if r != ReasonUnreachable {
			t.Fatalf("%s: reason %s (%v)", tg, r, err)
		}
		if want == "" {
			want = err.Error()
		}
		if err.Error() != want {
			t.Fatalf("%s: text %q differs from %q", tg, err.Error(), want)
		}
	}
	var wg sync.WaitGroup
	results := make([]string, 16)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := dialReason(cr, targets[i%len(targets)])
			results[i] = fmt.Sprintf("%s|%v", r, err)
		}()
	}
	wg.Wait()
	for i, got := range results {
		if got != fmt.Sprintf("%s|%s", ReasonUnreachable, want) {
			t.Fatalf("parallel dial %d: %q", i, got)
		}
	}
}

// A refusal from the exit reads as the same Refusal through the stream's verdict callback, which
// the SOCKS5 layer uses.
func TestClientVerdictCallbackCarriesReason(t *testing.T) {
	cr := newClientRig(t, rigConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := cr.cl.DialExitStream(ctx, "127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	vn, ok := conn.(core.VerdictNotifier)
	if !ok {
		t.Fatal("stream lost its verdict notifier")
	}
	got := make(chan error, 1)
	vn.OnVerdict(func(err error) { got <- err })
	select {
	case err := <-got:
		if r := ReasonOf(err); r != ReasonPolicy {
			t.Fatalf("verdict reason %s (%v)", r, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no verdict")
	}
	var _ io.Reader = conn
}

// T10.4 acceptance, finding 1: a deadline of the session set-up (the node's own 10 s readiness wait,
// the carrier's timeouts) with the caller's context still alive is a missing path, not an unknown
// error; only the caller's own cancellation stays as it is.
func TestClientInternalDeadlineIsNoPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cover bool
		want  Reason
	}{
		{"exit with a cover", false, ReasonNoPath},
		{"exit without a cover", true, ReasonNoCamouflage},
	} {
		for _, inner := range []error{context.DeadlineExceeded, context.Canceled} {
			t.Run(tc.name+"/"+inner.Error(), func(t *testing.T) {
				cfg := failingOpen(fmt.Errorf("session readiness: %w", inner))
				cfg.NoCamouflage = tc.cover
				cl, err := NewClient(cfg)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				conn, err := cl.DialExitStream(ctx, "192.0.2.1:80")
				if err == nil {
					defer conn.Close()
					_, err = conn.Read(make([]byte, 1))
				}
				if got := ReasonOf(err); got != tc.want {
					t.Fatalf("stream: reason %s (%v)", got, err)
				}
				_, err = cl.DialExitUDP(ctx)
				if got := ReasonOf(err); got != tc.want {
					t.Fatalf("udp: reason %s (%v)", got, err)
				}
			})
		}
	}
}

func TestClientCallersOwnCancellationIsNotARefusal(t *testing.T) {
	cl, err := NewClient(failingOpen(context.Canceled))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := cl.DialExitStream(ctx, "192.0.2.1:80")
	if err == nil {
		defer conn.Close()
		_, err = conn.Read(make([]byte, 1))
	}
	if r := ReasonOf(err); r != ReasonUnknown {
		t.Fatalf("a cancelled caller got reason %s (%v)", r, err)
	}
}

// T10.4 acceptance, finding 2: the codes are a contract with the facade and the frontend; they are
// fixed here, and a new reason goes at the end.
func TestReasonCodesAreFixed(t *testing.T) {
	want := map[Reason]string{
		ReasonUnknown: "unknown", ReasonNoCamouflage: "no-camouflage", ReasonUnreachable: "unreachable", ReasonBusy: "busy",
		ReasonPolicy: "policy", ReasonRateLimited: "rate-limited", ReasonNoPath: "no-path",
	}
	for r, code := range want {
		if r.String() != code {
			t.Errorf("reason %d is %q, want %q", r, r.String(), code)
		}
	}
	order := []Reason{ReasonUnknown, ReasonNoCamouflage, ReasonUnreachable, ReasonBusy, ReasonPolicy, ReasonRateLimited, ReasonNoPath}
	for i, r := range order {
		if int(r) != i {
			t.Errorf("%s moved to %d, want %d: new reasons go at the end", r, r, i)
		}
	}
}
