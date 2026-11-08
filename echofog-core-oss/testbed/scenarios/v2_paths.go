package scenarios

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/testbed/behaviour"
	"github.com/echofog/echofog-core/testbed/gate"
)

// maxPause is the longest wait a user may feel when a path is cut and another carries on (T15.0c).
const maxPause = 10 * time.Second

func pathsScenario() Scenario {
	return Scenario{Name: "v2-paths", Title: "Paths: a second path, a cut, an address change, the H15 backoff", Live: livePaths, Run: runPaths}
}

// newRow is a row of a scenario; the source of the limit is the task unless a row says otherwise.
func newRow(id, metric, value, limit string, st gate.Status, note string) gate.Row {
	return gate.Row{ID: id, Metric: metric, Value: value, Limit: limit, Status: st, Source: "T15.1", Note: note}
}

func attemptsOf(a ...carrier.Attempt) []carrier.Attempt { return a }

// pathsWorld is a client and a server that listens on one or two noise3 addresses, each behind its
// own netem; the echo service is on both.
func pathsWorld(dir string, second bool) (*World, *Node, *Node, error) {
	w := NewWorld(dir)
	spec := NodeSpec{Name: "srv", Server: true, Listen: true, Netem: &behaviour.NetemConfig{Seed: 1}}
	if second {
		spec.More = []*behaviour.NetemConfig{{Seed: 2}}
	}
	srv, err := w.Add(spec)
	if err != nil {
		w.Close()
		return nil, nil, nil, err
	}
	cli, err := w.Add(NodeSpec{Name: "cli"})
	if err != nil {
		w.Close()
		return nil, nil, nil, err
	}
	RegisterEcho(srv)
	RegisterEcho(cli)
	return w, cli, srv, nil
}

func runPaths(ctx context.Context, env Env) Result {
	res := Result{Name: "v2-paths", Mode: "model", Seed: env.Seed}
	dir, err := os.MkdirTemp(env.Dir, "paths-")
	if err != nil {
		dir = os.TempDir()
	}
	defer os.RemoveAll(dir)

	res.Rows = append(res.Rows, secondPathRow(ctx, dir))
	res.Rows = append(res.Rows, cutRows(ctx, dir, true)...)
	res.Rows = append(res.Rows, addressChangeRow(ctx, dir))

	// H15 and the carrier limit are read from the behaviour module on the gate's own limits: the
	// scenario keeps no copy of them.
	pen := behaviour.MeasurePenalty(0, 2*time.Hour)
	par := behaviour.MeasureParallel(behaviour.CensorConfig{}, 50, false)
	for _, g := range gate.BehaviourRows(&behaviour.Report{PenaltyNever: pen, Parallel: map[string]behaviour.ParallelResult{"product": par}}) {
		switch g.ID {
		case "D24":
			g.ID = "v2-paths/h15-backoff"
		case "D21":
			g.ID = "v2-paths/two-carriers"
		default:
			continue
		}
		res.Rows = append(res.Rows, g)
	}
	res.NotChecked = []string{
		"a real router cut and a real NAT remapping: the netem cuts and rebinds UDP on loopback, which stands in for iptables and conntrack on the stand",
		"the H15 backoff is played on a virtual clock against session.PathPolicy, not against a censor dropping real handshakes",
	}
	return res
}

// secondPathRow dials the first address, adds a path through the second and wants both live.
func secondPathRow(ctx context.Context, dir string) gate.Row {
	const id = "v2-paths/second-path"
	fail := func(err error, note string) gate.Row {
		return newRow(id, "live paths", err.Error(), "2", gate.Red, note)
	}
	w, cli, srv, err := pathsWorld(dir, true)
	if err != nil {
		return fail(err, "the world did not start")
	}
	defer w.Close()
	if err := w.StartAll(ctx); err != nil {
		return fail(err, "start")
	}
	if _, _, err := cli.DialVia(ctx, srv, 0); err != nil {
		return fail(err, "dial")
	}
	if err := addSecondPath(ctx, cli, srv); err != nil {
		return fail(err, "AddPath")
	}
	ids, active, ok := cli.RT.Paths(srv.ID.DeviceID)
	st := gate.Green
	if !ok || len(ids) < 2 {
		st = gate.Red
	}
	return newRow(id, "live paths", fmt.Sprintf("%d (active %d)", len(ids), active), "2", st, "")
}

func addSecondPath(ctx context.Context, cli, srv *Node) error {
	card, err := srv.PeerCard()
	if err != nil {
		return err
	}
	at, err := cli.AttemptFor(srv, 1)
	if err != nil {
		return err
	}
	_, err = cli.RT.AddPath(ctx, srv.ID.DeviceID, node.DialOpt{Card: card, Attempts: attemptsOf(at)})
	return err
}

const streamLimit = "the stream finishes, pause <= 10 s"

// streamRow runs a 4 MiB echo and, a quarter of the way, calls act. The row is green when the
// stream finishes whole with a pause of at most maxPause.
func streamRow(ctx context.Context, id, metric string, cli, srv *Node, act func()) gate.Row {
	const total = 4 << 20
	var once sync.Once
	cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	run := cli.EchoStream(cctx, srv, EchoOptions{Total: total, Chunk: 16 << 10, Pace: 2 * time.Millisecond, Hook: func(sent int64) {
		if sent >= total/4 {
			once.Do(act)
		}
	}})
	value := fmt.Sprintf("%d of %d bytes, longest pause %s", run.Received, run.Sent, run.MaxPause.Round(time.Millisecond))
	switch {
	case run.Err != nil:
		return newRow(id, metric, value, streamLimit, gate.Red, run.Err.Error())
	case run.MaxPause > maxPause:
		return newRow(id, metric, value, streamLimit, gate.Red, "the pause is longer than a user should feel")
	}
	return newRow(id, metric, value, streamLimit, gate.Green, "")
}

// cutRows runs a paced echo through the first path, cuts that path a quarter of the way, and then
// asks what a user would: does a new stream work, and how soon? The session has to survive on the
// second path and a new stream has to echo within maxPause of the cut. Without a second path it
// cannot, which is what makes the row a measurement. What happens to the stream that was in flight
// is reported on its own row: the mux has no acknowledgements, so bytes that were on the dead path
// are gone, and the resumption belongs to the service above (files, T8.1b).
func cutRows(ctx context.Context, dir string, second bool) []gate.Row {
	id := "v2-paths/cut-failover"
	if !second {
		id = "v2-paths/cut-control"
	}
	const limit = "a new stream echoes within 10 s of the cut"
	fail := func(err error, note string) []gate.Row {
		return []gate.Row{newRow(id, "cut", err.Error(), limit, gate.Red, note)}
	}
	w, cli, srv, err := pathsWorld(dir, second)
	if err != nil {
		return fail(err, "the world did not start")
	}
	defer w.Close()
	if err := w.StartAll(ctx); err != nil {
		return fail(err, "start")
	}
	if _, _, err := cli.DialVia(ctx, srv, 0); err != nil {
		return fail(err, "dial")
	}
	if second {
		if err := addSecondPath(ctx, cli, srv); err != nil {
			return fail(err, "AddPath")
		}
	}

	const total = 4 << 20
	var cutAt atomic.Int64
	aCtx, stopA := context.WithTimeout(ctx, 30*time.Second)
	defer stopA()
	aDone := make(chan EchoRun, 1)
	go func() {
		aDone <- cli.EchoStream(aCtx, srv, EchoOptions{Total: total, Chunk: 16 << 10, Pace: 2 * time.Millisecond, Hook: func(sent int64) {
			if sent >= total/4 && cutAt.CompareAndSwap(0, time.Now().UnixNano()) {
				srv.Netems[0].Cut(time.Hour)
			}
		}})
	}()
	for cutAt.Load() == 0 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			return fail(ctx.Err(), "the cut never came")
		}
	}
	cutTime := time.Unix(0, cutAt.Load())
	var recovered time.Duration
	var lastErr error
	for time.Since(cutTime) < 3*maxPause && recovered == 0 {
		bCtx, stopB := context.WithTimeout(ctx, 3*time.Second)
		b := cli.EchoStream(bCtx, srv, EchoOptions{Total: 64 << 10})
		stopB()
		if b.Err == nil {
			recovered = time.Since(cutTime)
		} else {
			lastErr = b.Err
		}
	}
	rows := make([]gate.Row, 0, 2)
	switch {
	case recovered == 0:
		rows = append(rows, newRow(id, "cut", fmt.Sprintf("no new stream within %s", (3*maxPause).Round(time.Second)), limit, gate.Red, fmt.Sprint(lastErr)))
	case recovered > maxPause:
		rows = append(rows, newRow(id, "cut", recovered.Round(time.Millisecond).String(), limit, gate.Red, "the failover took longer than a user should wait"))
	default:
		rows = append(rows, newRow(id, "cut", recovered.Round(time.Millisecond).String(), limit, gate.Green, ""))
	}
	if second {
		var a EchoRun
		select {
		case a = <-aDone:
		case <-time.After(15 * time.Second):
			stopA()
			a = <-aDone
		}
		value := fmt.Sprintf("%d of %d bytes returned", a.Received, a.Sent)
		if a.Err == nil {
			rows = append(rows, newRow("v2-paths/inflight-stream", "stream in flight", value, "reported, not gated", gate.Info, "the stream survived the cut"))
		} else {
			rows = append(rows, newRow("v2-paths/inflight-stream", "stream in flight", value, "reported, not gated", gate.Info,
				"finding: the stream in flight on the cut path did not continue on the second path ("+a.Err.Error()+"); resumption is the service's job"))
		}
	}
	return rows
}

// pathsCutWithoutSecondPath is the control of the cut row: one path, cut for good.
func pathsCutWithoutSecondPath(ctx context.Context, dir string) gate.Row {
	d, _ := os.MkdirTemp(dir, "paths-control-")
	defer removeTree(d)
	return cutRows(ctx, d, false)[0]
}

// addressChangeRow rebinds the netem's client side a quarter of the way: the server sees the same
// device from a new source address, as it does behind a NAT that remapped the flow.
func addressChangeRow(ctx context.Context, dir string) gate.Row {
	const id = "v2-paths/address-change"
	w, cli, srv, err := pathsWorld(dir, false)
	if err != nil {
		return newRow(id, "address change", err.Error(), streamLimit, gate.Red, "the world did not start")
	}
	defer w.Close()
	if err := w.StartAll(ctx); err != nil {
		return newRow(id, "address change", err.Error(), streamLimit, gate.Red, "start")
	}
	if _, _, err := cli.DialVia(ctx, srv, 0); err != nil {
		return newRow(id, "address change", err.Error(), streamLimit, gate.Red, "dial")
	}
	return streamRow(ctx, id, "address change", cli, srv, func() { srv.Netems[0].Rebind() })
}
