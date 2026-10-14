package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/p2p/routing"
	api "github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/node"
)

// Status is what a running node reports about itself.
type Status struct {
	Version   string             `json:"version"`
	PeerID    string             `json:"peerId"`
	JoinAddr  string             `json:"joinAddr"`
	NAT       string             `json:"nat"`
	StartedAt int64              `json:"startedAt"`
	UpdatedAt int64              `json:"updatedAt"`
	Peers     int                `json:"peers"`
	Relay     routing.RelayStats `json:"relay"`
	Exit      *ExitStatus        `json:"exit,omitempty"`
	Wallet    *WalletStatus      `json:"wallet,omitempty"`
}

type ExitStatus struct {
	Enabled    bool   `json:"enabled"`
	PricePerGB string `json:"pricePerGb"` // dollars
	Sessions   uint64 `json:"sessions"`
	Refused    uint64 `json:"refused"`
	Carried    uint64 `json:"carriedBytes"`
	EarnedUSD  string `json:"earnedUsd"` // credited from clients since the exit started, TON valued on arrival
	LastError  string `json:"lastError,omitempty"`
}

type WalletStatus struct {
	Address string  `json:"address"`
	Network string  `json:"network"`
	TON     string  `json:"ton"`
	USDT    string  `json:"usdt,omitempty"`
	USDRate float64 `json:"usdRate,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// collector refreshes the status now and then; the balance comes from the network, so it is not
// read on every request.
type collector struct {
	n       *node.Node
	h       *api.NodeApi
	o       options
	started time.Time

	mu        sync.Mutex
	last      Status
	balanceAt time.Time // the balance is read from the network, so not on every round
}

func newCollector(n *node.Node, h *api.NodeApi, o options, started time.Time) *collector {
	return &collector{n: n, h: h, o: o, started: started}
}

func (c *collector) snapshot() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *collector) run(ctx context.Context) {
	for {
		c.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (c *collector) refresh(ctx context.Context) {
	st := Status{
		Version:   version,
		PeerID:    c.n.ID.String(),
		JoinAddr:  joinAddress(c.n, c.o.listenAddr),
		StartedAt: c.started.Unix(),
		UpdatedAt: time.Now().Unix(),
		Peers:     len(c.h.GetPeers()),
	}
	if c.n.NAT != nil {
		_, kind := c.n.NAT.GetNATProfile()
		st.NAT = kind.String()
	}
	if rs := c.n.GetRelayServer(); rs != nil {
		st.Relay = rs.Stats()
	}
	if c.o.exit {
		sh := c.h.GetSharingStatus()
		st.Exit = &ExitStatus{
			Enabled: sh.Enabled, PricePerGB: c.o.exitPrice, Sessions: sh.Accepted, Refused: sh.Refused,
			Carried: sh.Bytes, EarnedUSD: sh.EarnedUSDT, LastError: sh.LastError,
		}
	}
	if w, err := c.h.GetTonWallet(); err == nil {
		ws := &WalletStatus{Address: w.Address, Network: w.Network}
		c.mu.Lock()
		prev, fresh := c.last.Wallet, time.Since(c.balanceAt) < 2*time.Minute
		c.mu.Unlock()
		if fresh && prev != nil {
			st.Wallet = prev
			c.mu.Lock()
			c.last = st
			c.mu.Unlock()
			return
		}
		bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if b, err := c.h.GetTonBalance(bctx); err == nil {
			ws.TON, ws.USDRate = b.TON, b.USDRate
			if b.USDTAvailable {
				ws.USDT = b.USDT
			}
		} else {
			ws.Error = err.Error()
			if prev := c.snapshot().Wallet; prev != nil { // keep the last known balance
				ws.TON, ws.USDT, ws.USDRate = prev.TON, prev.USDT, prev.USDRate
			}
		}
		cancel()
		st.Wallet = ws
		c.mu.Lock()
		c.balanceAt = time.Now()
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.last = st
	c.mu.Unlock()
}

// logEvery prints one line with what the node does, for `docker logs` and journald.
func (c *collector) logEvery(ctx context.Context, every time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		fmt.Println(statsLine(c.snapshot()))
	}
}

func statsLine(s Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s stats peers=%d", time.Now().Format("2006-01-02 15:04:05"), s.Peers)
	r := s.Relay
	if r.Enabled && r.SignalingOnly {
		fmt.Fprintf(&b, " | signaling: %d/%d active, %d opened, %s passed on, %d traffic frames and %d messages refused",
			r.ActiveCircuits, r.MaxCircuits, r.CircuitsOpened, human(r.BytesRouted), r.SignalRefused, r.RefusedMessages)
	} else if r.Enabled {
		kind := "relay"
		if r.Private {
			kind = "private relay"
		}
		fmt.Fprintf(&b, " | %s: %d/%d active, %d opened, %s routed, %d msgs passed on, %d kept for offline (%d waiting, %d delivered)",
			kind, r.ActiveCircuits, r.MaxCircuits, r.CircuitsOpened, human(r.BytesRouted), r.Forwarded, r.Stored, r.MailboxWaiting, r.Delivered)
		if r.Private {
			fmt.Fprintf(&b, ", %d allowed, %d refused", r.AllowedPeers, r.AccessRefused)
		}
	} else {
		b.WriteString(" | relay: off")
	}
	if e := s.Exit; e != nil {
		fmt.Fprintf(&b, " | exit: %d sessions, %s carried, earned $%s", e.Sessions, human(e.Carried), orZero(e.EarnedUSD))
		if e.LastError != "" {
			fmt.Fprintf(&b, " (%s)", e.LastError)
		}
	}
	if w := s.Wallet; w != nil && w.TON != "" {
		fmt.Fprintf(&b, " | wallet: %s TON", w.TON)
		if w.USDT != "" {
			fmt.Fprintf(&b, ", %s USDT", w.USDT)
		}
	}
	return b.String()
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func human(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// serveStatus answers GET /status on a local address with the latest snapshot.
func serveStatus(ctx context.Context, addr string, c *collector) error {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); host != "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			fmt.Printf("Note: the status endpoint listens on %s, not on loopback: it shows the wallet address and earnings to whoever can reach it\n", addr)
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.snapshot())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if c.snapshot().PeerID == "" {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// statusCmd asks the running node for its status and prints it.
func statusCmd(args []string, walletOnly bool) int {
	name := "status"
	if walletOnly {
		name = "wallet"
	}
	f := flag.NewFlagSet(name, flag.ExitOnError)
	addr := f.String("status-addr", getEnv("ECHOFOG_STATUS_ADDR", "127.0.0.1:7070"), "status endpoint of the running node")
	asJSON := f.Bool("json", false, "print the raw JSON")
	quiet := f.Bool("quiet", false, "print nothing; the exit code tells whether the node answers (for health checks)")
	_ = f.Parse(args)

	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get("http://" + *addr + "/status")
	if err != nil {
		if !*quiet {
			fmt.Fprintf(os.Stderr, "the node does not answer on %s: is it running (echofog-node run)? %v\n", *addr, err)
		}
		return 1
	}
	defer res.Body.Close()
	var s Status
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil || s.PeerID == "" {
		if !*quiet {
			fmt.Fprintln(os.Stderr, "the node is still starting")
		}
		return 1
	}
	if *quiet {
		return 0
	}
	if *asJSON {
		out, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println(string(out))
		return 0
	}
	if walletOnly {
		printWallet(s)
		return 0
	}
	printStatus(s)
	return 0
}

func printStatus(s Status) {
	fmt.Printf("EchoFog node %s, up %s\n", s.Version, time.Since(time.Unix(s.StartedAt, 0)).Round(time.Second))
	fmt.Printf("  Peer ID:      %s\n", s.PeerID)
	fmt.Printf("  Join address: %s\n", s.JoinAddr)
	if s.NAT != "" {
		fmt.Printf("  NAT:          %s\n", s.NAT)
	}
	fmt.Printf("  Peers:        %d\n", s.Peers)
	r := s.Relay
	if r.Enabled && r.SignalingOnly {
		fmt.Println("  Signaling:    DHT and hole punching help, no traffic relayed")
		fmt.Printf("    connections   %d active of %d, %d opened since start, %s of signals passed on\n", r.ActiveCircuits, r.MaxCircuits, r.CircuitsOpened, human(r.BytesRouted))
		fmt.Printf("    refused       %d traffic frames, %d messages to relay or keep\n", r.SignalRefused, r.RefusedMessages)
	} else if r.Enabled {
		if r.Private {
			fmt.Println("  Relay:        private (not advertised)")
			fmt.Printf("    access        %d allowed peer(s), %d request(s) refused\n", r.AllowedPeers, r.AccessRefused)
		} else {
			fmt.Println("  Relay:        public")
		}
		fmt.Printf("    connections   %d active of %d, %d opened since start\n", r.ActiveCircuits, r.MaxCircuits, r.CircuitsOpened)
		fmt.Printf("    traffic       %s routed for others\n", human(r.BytesRouted))
		fmt.Printf("    messages      %d passed on, %d kept for offline peers (%d waiting, %d delivered)\n", r.Forwarded, r.Stored, r.MailboxWaiting, r.Delivered)
	} else {
		fmt.Println("  Relay:        off")
	}
	if e := s.Exit; e != nil {
		state := "sharing"
		if !e.Enabled {
			state = "not running"
		}
		fmt.Printf("  Exit:         %s, $%s per GB\n", state, e.PricePerGB)
		fmt.Printf("    sessions      %d accepted, %d refused\n", e.Sessions, e.Refused)
		fmt.Printf("    traffic       %s carried\n", human(e.Carried))
		fmt.Printf("    earned        $%s\n", orZero(e.EarnedUSD))
		if e.LastError != "" {
			fmt.Printf("    last error    %s\n", e.LastError)
		}
	}
	if s.Wallet != nil {
		printWallet(s)
	}
	fmt.Printf("  (updated %s ago)\n", time.Since(time.Unix(s.UpdatedAt, 0)).Round(time.Second))
}

func printWallet(s Status) {
	w := s.Wallet
	if w == nil {
		fmt.Println("  Wallet:       not available")
		return
	}
	fmt.Printf("  Wallet (%s): %s\n", w.Network, w.Address)
	if w.TON != "" {
		line := w.TON + " TON"
		if w.USDT != "" {
			line += ", " + w.USDT + " USDT"
		}
		if w.USDRate > 0 {
			line += fmt.Sprintf("  (1 TON = $%.2f)", w.USDRate)
		}
		fmt.Printf("    balance       %s\n", line)
	}
	if w.Error != "" {
		fmt.Printf("    note          balance not refreshed: %s\n", w.Error)
	}
}
