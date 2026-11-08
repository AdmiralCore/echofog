package scenarios

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/testbed/behaviour"
	"github.com/echofog/echofog-core/testbed/gate"
)

func exitScenario() Scenario {
	return Scenario{Name: "v2-exit", Title: "Exit: a download through the exit, whole, and the exit gives its resources back", Live: liveExit, Run: runExit}
}

// labWeb is the web server that stands in for the Internet: it serves deterministic bytes with their
// SHA-256 in a header, so a client can tell a whole download from a damaged one. It counts the
// connections it accepted.
type labWeb struct {
	ln       net.Listener
	srv      *http.Server
	accepted atomic.Int64
}

func startLabWeb() (*labWeb, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	w := &labWeb{}
	w.ln = countingListener{ln, &w.accepted}
	mux := http.NewServeMux()
	mux.HandleFunc("/file", func(rw http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("kb"))
		data := make([]byte, n<<10)
		rand.New(rand.NewSource(int64(n))).Read(data)
		sum := sha256.Sum256(data)
		rw.Header().Set("X-SHA256", hex.EncodeToString(sum[:]))
		rw.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = rw.Write(data)
	})
	w.srv = &http.Server{Handler: mux}
	go func() { _ = w.srv.Serve(w.ln) }()
	return w, nil
}

func (w *labWeb) addr() string { return w.ln.Addr().String() }
func (w *labWeb) close()       { _ = w.srv.Close() }

type countingListener struct {
	net.Listener
	n *atomic.Int64
}

func (c countingListener) Accept() (net.Conn, error) {
	conn, err := c.Listener.Accept()
	if err == nil {
		c.n.Add(1)
	}
	return conn, err
}

// fetch downloads /file?kb=N through the exit tunnel to target and checks the bytes against the
// hash the server sent.
func fetch(ctx context.Context, cl *exit.Client, target string, kb int) (got int, err error) {
	conn, err := cl.DialExitStream(ctx, target)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { go conn.Close() })
	defer stop()
	if _, err := fmt.Fprintf(conn, "GET /file?kb=%d HTTP/1.0\r\nHost: lab\r\n\r\n", kb); err != nil {
		return 0, err
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		return len(raw), err
	}
	i := bytes.Index(raw, []byte("\r\n\r\n"))
	if i < 0 {
		return len(raw), fmt.Errorf("no HTTP answer (%d bytes)", len(raw))
	}
	head, body := string(raw[:i]), raw[i+4:]
	sum := sha256.Sum256(body)
	if want := headerValue(head, "X-SHA256"); want != hex.EncodeToString(sum[:]) {
		return len(body), fmt.Errorf("the download is damaged: %d bytes, hash differs", len(body))
	}
	if len(body) != kb<<10 {
		return len(body), fmt.Errorf("the download is short: %d of %d bytes", len(body), kb<<10)
	}
	return len(body), nil
}

func headerValue(head, name string) string {
	for _, line := range strings.Split(head, "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func runExit(ctx context.Context, env Env) Result {
	res := Result{Name: "v2-exit", Mode: "model", Seed: env.Seed}
	dir, err := os.MkdirTemp(env.Dir, "exit-")
	if err != nil {
		dir = os.TempDir()
	}
	defer os.RemoveAll(dir)
	web, err := startLabWeb()
	if err != nil {
		res.Rows = append(res.Rows, newRow("v2-exit/download", "web server", err.Error(), "up", gate.Red, ""))
		return res
	}
	defer web.close()
	kb := 8 << 10
	if env.Short {
		kb = 2 << 10
	}

	w := NewWorld(dir)
	defer w.Close()
	lossy := &behaviour.NetemConfig{Loss: 0.03, Delay: 20 * time.Millisecond, Seed: env.Seed}
	x1, err := w.Add(NodeSpec{Name: "x1", Server: true, Listen: true, LabExit: true, Netem: lossy})
	if err != nil {
		res.Rows = append(res.Rows, newRow("v2-exit/download", "exit", err.Error(), "up", gate.Red, ""))
		return res
	}
	x2, _ := w.Add(NodeSpec{Name: "x2", Server: true, Listen: true, LabExit: true})
	prod, _ := w.Add(NodeSpec{Name: "prod", Server: true, Listen: true, Exit: true})
	cli, _ := w.Add(NodeSpec{Name: "cli"})
	if x2 == nil || prod == nil || cli == nil {
		res.Rows = append(res.Rows, newRow("v2-exit/download", "world", "a node did not build", "up", gate.Red, ""))
		return res
	}
	if err := w.StartAll(ctx); err != nil {
		res.Rows = append(res.Rows, newRow("v2-exit/download", "world", err.Error(), "up", gate.Red, "start"))
		return res
	}

	open := func(x *Node, within time.Duration) (*exit.Client, error) {
		c, cancel := context.WithTimeout(ctx, within)
		defer cancel()
		card, err := x.PeerCard()
		if err != nil {
			return nil, err
		}
		return cli.RT.ExitClient(c, card, node.DialOpt{Card: card})
	}

	// 1. a download through the lossy exit
	{
		const id = "v2-exit/download"
		cl, err := open(x1, 30*time.Second)
		if err != nil {
			res.Rows = append(res.Rows, newRow(id, "download", err.Error(), "whole, hash equal", gate.Red, "no exit client"))
		} else {
			dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			start := time.Now()
			n, err := fetch(dctx, cl, web.addr(), kb)
			cancel()
			if err != nil {
				res.Rows = append(res.Rows, newRow(id, "download", err.Error(), "whole, hash equal", gate.Red, ""))
			} else {
				mbps := float64(n*8) / time.Since(start).Seconds() / 1e6
				res.Rows = append(res.Rows, newRow(id, "download", fmt.Sprintf("%d KiB whole, %.1f Mbit/s, 3 %% loss, 20 ms", n>>10, mbps), "whole, hash equal", gate.Green, ""))
			}
			_ = cl.Close()
		}
	}

	// 2. the product's exit module refuses a loopback target
	{
		const id = "v2-exit/private-refused"
		before := web.accepted.Load()
		cl, err := open(prod, 30*time.Second)
		if err != nil {
			res.Rows = append(res.Rows, newRow(id, "private target", err.Error(), "refused, the web server never sees it", gate.Red, "no exit client"))
		} else {
			dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, ferr := fetch(dctx, cl, web.addr(), 1)
			cancel()
			_ = cl.Close()
			seen := web.accepted.Load() - before
			switch {
			case ferr == nil || seen != 0:
				res.Rows = append(res.Rows, newRow(id, "private target", fmt.Sprintf("answered, the web server saw %d connections", seen), "refused, the web server never sees it", gate.Red, "SSRF: the exit reached a loopback address"))
			default:
				res.Rows = append(res.Rows, newRow(id, "private target", "refused: "+ferr.Error(), "refused, the web server never sees it", gate.Green, ""))
			}
		}
	}

	// 3. the first exit goes away: the client takes the second
	{
		const id = "v2-exit/failover"
		x1.Netem.Cut(time.Hour)
		start := time.Now()
		got := fmt.Errorf("not tried")
		// the client keeps its session with x1 until it tries to use it
		stale := func() error {
			cl, err := open(x1, 8*time.Second)
			if err != nil {
				return err
			}
			defer cl.Close()
			fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			_, err = fetch(fctx, cl, web.addr(), 1)
			return err
		}
		if err := stale(); err == nil {
			got = fmt.Errorf("the cut exit still answered")
		} else if cl, err := open(x2, 30*time.Second); err != nil {
			got = err
		} else {
			dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			_, got = fetch(dctx, cl, web.addr(), 256)
			cancel()
			_ = cl.Close()
		}
		took := time.Since(start)
		switch {
		case got != nil:
			res.Rows = append(res.Rows, newRow(id, "failover", got.Error(), "a download through the second exit within 60 s", gate.Red, ""))
		case took > 60*time.Second:
			res.Rows = append(res.Rows, newRow(id, "failover", took.Round(time.Second).String(), "within 60 s", gate.Red, ""))
		default:
			res.Rows = append(res.Rows, newRow(id, "failover", took.Round(time.Millisecond).String(), "a download through the second exit within 60 s", gate.Green, ""))
		}
	}

	// 4. many tunnels, then the process gives its goroutines back
	{
		const id = "v2-exit/resources"
		cl, err := open(x2, 30*time.Second)
		if err != nil {
			res.Rows = append(res.Rows, newRow(id, "goroutines", err.Error(), "back to where they were", gate.Red, "no exit client"))
		} else {
			settle := func() int {
				time.Sleep(3 * time.Second)
				runtime.GC()
				return runtime.NumGoroutine()
			}
			for i := 0; i < 3; i++ { // warm up
				_, _ = fetch(ctx, cl, web.addr(), 4)
			}
			before := settle()
			const tunnels = 60
			failed := 0
			var firstErr error
			for i := 0; i < tunnels; i++ {
				fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				if _, err := fetch(fctx, cl, web.addr(), 4); err != nil {
					failed++
					if firstErr == nil {
						firstErr = fmt.Errorf("tunnel %d: %w", i, err)
					}
				}
				cancel()
				time.Sleep(100 * time.Millisecond) // the exit lets 20 new tunnels a second through; faster is refused as rate-limited
			}
			after := settle()
			value := fmt.Sprintf("%d tunnels, %d failed, goroutines %d to %d", tunnels, failed, before, after)
			st, note := gate.Green, ""
			if failed > 0 || after > before+10 {
				st = gate.Red
				if firstErr != nil {
					note = firstErr.Error()
				}
			}
			res.Rows = append(res.Rows, newRow(id, "goroutines", value, "no growth beyond 10, no failures", st, note))
			_ = cl.Close()
		}
	}
	res.NotChecked = []string{
		"the exit's payment, vouchers and limits (T10.3): the lab exit is free and unlimited",
		"DNS through the exit (DoH) and UDP through the exit: the web server is addressed by IP",
		"goroutines are counted for the whole process, which holds the client, the exit and the world together; sockets and memory of the exit alone are measured on the stand",
	}
	return res
}
