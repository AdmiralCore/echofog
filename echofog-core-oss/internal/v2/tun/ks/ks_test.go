package ks_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/ks"
	"github.com/echofog/echofog-core/internal/v2/tun/tuntest"
)

type world struct {
	t       *testing.T
	kernel  *tuntest.Kernel
	fw      *tuntest.Firewall
	tn      *tuntest.Tunnel
	dns     *tuntest.DNS
	j       *tuntest.Journal
	client  *ks.Client
	connect atomic.Int32
	served  chan error
	mu      sync.Mutex
	srvConn []net.Conn
}

// newWorld wires a Client to a Server over a loopback connection, as the connector would.
func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, kernel: &tuntest.Kernel{}, j: &tuntest.Journal{}, served: make(chan error, 8)}
	w.fw = &tuntest.Firewall{K: w.kernel, J: w.j}
	w.tn = tuntest.NewTunnel()
	w.tn.J = w.j
	w.dns = &tuntest.DNS{J: w.j}
	b := ks.Backend{Firewall: w.fw, Tunnel: w.tn, DNS: w.dns}
	w.client = ks.NewClient(ks.ClientConfig{
		Connect: func(ctx context.Context) (net.Conn, error) {
			w.connect.Add(1)
			a, bconn := net.Pipe()
			w.mu.Lock()
			w.srvConn = append(w.srvConn, bconn)
			w.mu.Unlock()
			go func() { w.served <- ks.Serve(context.Background(), bconn, b) }()
			return a, nil
		},
		Installed: func(ctx context.Context) (bool, error) { return w.fw.Installed(ctx) },
	})
	t.Cleanup(func() { _ = w.client.Close() })
	return w
}

func (w *world) killHelper() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.srvConn {
		_ = c.Close()
	}
}

func plan() tun.Plan {
	return tun.Plan{Endpoints: []tun.Endpoint{{Addr: netip.MustParseAddrPort("203.0.113.10:443"), Proto: tun.ProtoUDP}},
		TunnelPool: []netip.Prefix{netip.MustParsePrefix("198.18.0.0/15")}}
}

func rules(t testing.TB) []tun.Rule {
	t.Helper()
	r, err := tun.BuildRules(plan())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var ctx = context.Background()

func TestRoundTripOfEveryOperation(t *testing.T) {
	w := newWorld(t)
	if err := w.client.Install(ctx, rules(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := w.kernel.Rules(); len(got) != len(rules(t)) {
		t.Fatalf("kernel has %d rules, want %d", len(got), len(rules(t)))
	}
	if ok, err := w.client.Installed(ctx); err != nil || !ok {
		t.Fatalf("Installed = %v %v", ok, err)
	}
	cfg := tun.TunConfig{Name: "echofog0", Resolver: netip.MustParseAddr("198.18.0.2"), Socks: "127.0.0.1:1080", BypassUDPPort: 4433,
		Addrs: []netip.Prefix{netip.MustParsePrefix("198.18.0.1/15")}}
	if err := w.client.Up(ctx, cfg); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !w.tn.IsUp() || w.tn.LastConf.Socks != "127.0.0.1:1080" || w.tn.LastConf.BypassUDPPort != 4433 || w.tn.LastConf.Resolver != cfg.Resolver {
		t.Fatalf("tunnel config on the helper side: %+v", w.tn.LastConf)
	}
	if !w.client.Alive(ctx) {
		t.Fatal("Alive = false with the tunnel up")
	}
	dcfg := tun.DNSConfig{Tunnel: "echofog0", Resolver: cfg.Resolver, DisableSmartNameResolution: true}
	if err := w.client.Protect(ctx, dcfg); err != nil || !w.dns.IsProtected() || w.dns.Last != dcfg {
		t.Fatalf("Protect: %v %+v", err, w.dns.Last)
	}
	if err := w.client.Restore(ctx); err != nil || w.dns.IsProtected() {
		t.Fatalf("Restore: %v", err)
	}
	if err := w.client.Down(ctx); err != nil || w.tn.IsUp() {
		t.Fatalf("Down: %v", err)
	}
	if err := w.client.Remove(ctx); err != nil || len(w.kernel.Rules()) != 0 {
		t.Fatalf("Remove: %v", err)
	}
	if w.connect.Load() != 1 {
		t.Fatalf("the helper was started %d times for one session", w.connect.Load())
	}
}

func TestErrorsKeepTheirMeaning(t *testing.T) {
	w := newWorld(t)
	w.fw.Unprivileged = true
	if err := w.client.Install(ctx, rules(t)); !errors.Is(err, tun.ErrNoPrivilege) {
		t.Fatalf("Install = %v, want ErrNoPrivilege", err)
	}
	w.fw.Unprivileged = false
	w.fw.FailInstall = errors.New("bfe busy")
	err := w.client.Install(ctx, rules(t))
	if err == nil || !strings.Contains(err.Error(), "bfe busy") {
		t.Fatalf("Install = %v: the reason must reach the application", err)
	}
	w.tn.FailUp = errors.New("no wintun")
	if err := w.client.Up(ctx, tun.TunConfig{Name: "x"}); err == nil || !strings.Contains(err.Error(), "no wintun") {
		t.Fatalf("Up = %v", err)
	}
	w.fw.FailRemove = errors.New("denied")
	if err := w.client.Remove(ctx); err == nil {
		t.Fatal("Remove reported success although the helper failed")
	}
}

// The helper checks the rules on its own: a connection that sends an open set is refused, whatever
// the client library does.
func TestTheHelperRefusesAnOpenRuleSet(t *testing.T) {
	w := newWorld(t)
	a, b := net.Pipe()
	go func() { _ = ks.Serve(ctx, b, ks.Backend{Firewall: w.fw, Tunnel: w.tn, DNS: w.dns}) }()
	open := []tun.Rule{{Name: "permit-all", Action: tun.Permit, Dir: tun.Out, Weight: 10, Persistent: true}}
	req, _ := json.Marshal(map[string]any{"id": 1, "op": "fw.install", "rules": open})
	_ = a.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = a.Write(append(req, '\n'))
	line, err := bufio.NewReader(a).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		ID  uint64
		Err string
	}
	if json.Unmarshal([]byte(line), &resp) != nil || resp.Err == "" {
		t.Fatalf("answer %q: an open rule set was accepted", line)
	}
	if w.fw.Installs != 0 || len(w.kernel.Rules()) != 0 {
		t.Fatalf("the firewall driver was called: %d installs", w.fw.Installs)
	}
	_ = a.Close()
}

func TestGarbageDoesNotCrashTheHelper(t *testing.T) {
	w := newWorld(t)
	for _, in := range []string{"not json\n", "{\"id\":1,\"op\":\"nope\"}\n", "{\"id\":\"x\"}\n", strings.Repeat("a", 5<<20) + "\n", "\x00\x01\x02\n"} {
		a, b := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- ks.Serve(ctx, b, ks.Backend{Firewall: w.fw, Tunnel: w.tn, DNS: w.dns}) }()
		_ = a.SetDeadline(time.Now().Add(3 * time.Second))
		go func() { _, _ = a.Write([]byte(in)); _, _ = bufio.NewReader(a).ReadString('\n') }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("Serve did not end on %.20q", in)
		}
		_ = a.Close()
	}
	if w.fw.Installs != 0 || w.fw.Removes != 0 || w.dns.Protects != 0 {
		t.Fatal("garbage reached a driver")
	}
}

func TestEventsOfTheTunnelReachTheApplication(t *testing.T) {
	w := newWorld(t)
	if err := w.client.Up(ctx, tun.TunConfig{Name: "echofog0"}); err != nil {
		t.Fatal(err)
	}
	w.tn.Kill(errors.New("adapter removed"))
	select {
	case ev := <-w.client.Events():
		if ev.Err == nil || !strings.Contains(ev.Err.Error(), "adapter removed") {
			t.Fatalf("event %v", ev.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
	}
}

// The helper dies (killed, crashed): the application sees a lost tunnel and Alive says no. The
// filters are the kernel's and stay. The next operation starts a new helper.
func TestHelperDeath(t *testing.T) {
	w := newWorld(t)
	if err := w.client.Install(ctx, rules(t)); err != nil {
		t.Fatal(err)
	}
	if err := w.client.Up(ctx, tun.TunConfig{Name: "echofog0"}); err != nil {
		t.Fatal(err)
	}
	w.killHelper()
	select {
	case ev := <-w.client.Events():
		if ev.Err == nil {
			t.Fatal("event without a reason")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the death of the helper was not reported")
	}
	select {
	case <-w.served: // the old helper has finished taking its tunnel down
	case <-time.After(3 * time.Second):
		t.Fatal("the old helper did not end")
	}
	if w.client.Alive(ctx) {
		t.Fatal("Alive after the helper died")
	}
	if len(w.kernel.Rules()) == 0 || w.fw.Removes != 0 {
		t.Fatal("the death of the helper touched the filters")
	}
	if ok, _ := w.client.Installed(ctx); !ok {
		t.Fatal("Installed says no while filters stand")
	}
	if err := w.client.Up(ctx, tun.TunConfig{Name: "echofog0"}); err != nil {
		t.Fatalf("Up after the death: %v", err)
	}
	if w.connect.Load() != 2 {
		t.Fatalf("helper starts: %d, want 2", w.connect.Load())
	}
	if !w.client.Alive(ctx) {
		t.Fatal("not alive after the new helper brought the tunnel up")
	}
}

// Down with no helper running has nothing to do and starts nothing (no prompt for rights).
func TestDownWithoutHelperStartsNothing(t *testing.T) {
	w := newWorld(t)
	if err := w.client.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if w.client.Alive(ctx) {
		t.Fatal("alive without a helper")
	}
	if ok, err := w.client.Installed(ctx); err != nil || ok {
		t.Fatalf("Installed = %v %v", ok, err)
	}
	if w.connect.Load() != 0 {
		t.Fatalf("%d helpers were started for Down, Alive and Installed", w.connect.Load())
	}
}

// The application goes away: the helper takes the tunnel down and touches nothing else.
func TestApplicationGoneLeavesTheFilters(t *testing.T) {
	w := newWorld(t)
	if err := w.client.Install(ctx, rules(t)); err != nil {
		t.Fatal(err)
	}
	if err := w.client.Up(ctx, tun.TunConfig{Name: "echofog0"}); err != nil {
		t.Fatal(err)
	}
	if err := w.client.Protect(ctx, tun.DNSConfig{Tunnel: "echofog0", Resolver: netip.MustParseAddr("198.18.0.2"), DisableSmartNameResolution: true}); err != nil {
		t.Fatal(err)
	}
	_ = w.client.Close()
	select {
	case <-w.served:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not end when the application went away")
	}
	if w.tn.IsUp() {
		t.Fatal("the tunnel stays up with no application to feed it")
	}
	if len(w.kernel.Rules()) == 0 || w.fw.Removes != 0 {
		t.Fatal("the filters were touched")
	}
	if !w.dns.IsProtected() || w.dns.Restores != 0 {
		t.Fatal("DNS protection was dropped")
	}
}

func TestCancelledContextReturns(t *testing.T) {
	w := newWorld(t)
	w.tn.UpGate = make(chan struct{})
	c, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- w.client.Up(c, tun.TunConfig{Name: "echofog0"}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Up does not return after cancel")
	}
}

func TestConcurrentOperations(t *testing.T) {
	w := newWorld(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = w.client.Install(ctx, rules(t))
				_, _ = w.client.Installed(ctx)
				_ = w.client.Alive(ctx)
			}
		}()
	}
	wg.Wait()
	if w.connect.Load() != 1 {
		t.Fatalf("helper starts: %d", w.connect.Load())
	}
}

// ---- the helper process: token, address, connector ------------------------------------------

func TestConnectorAndServeHelper(t *testing.T) {
	var built atomic.Int32
	b := ks.Backend{Firewall: &tuntest.Firewall{K: &tuntest.Kernel{}}, Tunnel: tuntest.NewTunnel(), DNS: &tuntest.DNS{}}
	launch := func(exe string, args []string) error {
		if exe != "echofog.exe" || len(args) < 1 || args[0] != ks.HelperFlag {
			t.Errorf("launch(%q, %v)", exe, args)
		}
		go func() {
			ks.ServeHelper(args[1:], func() (ks.Backend, func(), error) { built.Add(1); return b, func() {}, nil })
		}()
		return nil
	}
	conn := ks.Connector(launch, "echofog.exe", []string{"-x"}, 5*time.Second)
	c := ks.NewClient(ks.ClientConfig{Connect: conn})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Install(ctx, rules(t)); err != nil {
		t.Fatalf("Install through a launched helper: %v", err)
	}
	if built.Load() != 1 {
		t.Fatalf("backend built %d times", built.Load())
	}
}

// Another local process that finds the port first and sends the wrong token is skipped, not served.
func TestConnectorIgnoresAWrongToken(t *testing.T) {
	b := ks.Backend{Firewall: &tuntest.Firewall{K: &tuntest.Kernel{}}, Tunnel: tuntest.NewTunnel(), DNS: &tuntest.DNS{}}
	var impostorTried atomic.Bool
	launch := func(exe string, args []string) error {
		var addr string
		for i, a := range args {
			if a == "-ctl" {
				addr = args[i+1]
			}
		}
		go func() {
			c, err := net.Dial("tcp", addr)
			if err == nil {
				_, _ = c.Write([]byte("wrong-token\n"))
				impostorTried.Store(true)
				defer c.Close()
			}
			ks.ServeHelper(args[1:], func() (ks.Backend, func(), error) { return b, func() {}, nil })
		}()
		return nil
	}
	c := ks.NewClient(ks.ClientConfig{Connect: ks.Connector(launch, "e", nil, 5*time.Second)})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Install(ctx, rules(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !impostorTried.Load() {
		t.Fatal("the test did not try the wrong token")
	}
}

func TestConnectorTimesOut(t *testing.T) {
	launch := func(string, []string) error { return nil } // the helper never comes
	conn := ks.Connector(launch, "e", nil, 200*time.Millisecond)
	start := time.Now()
	if _, err := conn(ctx); err == nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("did not time out")
	}
	failing := ks.Connector(func(string, []string) error { return errors.New("declined") }, "e", nil, time.Second)
	if _, err := failing(ctx); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("launch error lost: %v", err)
	}
}

func TestServeHelperRefusesANonLoopbackAddress(t *testing.T) {
	if code := ks.ServeHelper([]string{"-ctl", "10.0.0.1:1", "-token", "t"}, func() (ks.Backend, func(), error) {
		t.Error("backend built for a non-loopback address")
		return ks.Backend{}, func() {}, nil
	}); code == 0 {
		t.Fatal("exit code 0")
	}
}
