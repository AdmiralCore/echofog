package behaviour

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func udpEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	return pc.LocalAddr().String()
}

func roundTrips(t *testing.T, via string, n, size int) (got int, rtt time.Duration) {
	t.Helper()
	c, err := net.Dial("udp", via)
	require.NoError(t, err)
	defer c.Close()
	buf := make([]byte, 4096)
	var total time.Duration
	for i := 0; i < n; i++ {
		start := time.Now()
		_, err := c.Write(make([]byte, size))
		require.NoError(t, err)
		_ = c.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		if _, err := c.Read(buf); err == nil {
			got++
			total += time.Since(start)
		}
	}
	if got > 0 {
		rtt = total / time.Duration(got)
	}
	return got, rtt
}

func TestNetemDropsDelaysAndCapsTheMTUDeterministically(t *testing.T) {
	echo := udpEcho(t)

	clean, err := StartNetem(echo, NetemConfig{Seed: 1})
	require.NoError(t, err)
	defer clean.Close()
	got, _ := roundTrips(t, clean.Addr(), 20, 200)
	assert.Equal(t, 20, got)

	slow, err := StartNetem(echo, NetemConfig{Delay: 40 * time.Millisecond, Seed: 1})
	require.NoError(t, err)
	defer slow.Close()
	_, rtt := roundTrips(t, slow.Addr(), 5, 200)
	assert.GreaterOrEqual(t, rtt, 80*time.Millisecond, "the delay applies in both directions")
	assert.Less(t, rtt, 200*time.Millisecond)

	lossy, err := StartNetem(echo, NetemConfig{Loss: 0.5, Seed: 7})
	require.NoError(t, err)
	defer lossy.Close()
	got, _ = roundTrips(t, lossy.Addr(), 60, 200)
	assert.Greater(t, got, 5)
	assert.Less(t, got, 45, "half of each direction is gone")
	assert.Greater(t, lossy.Stats().Dropped, int64(10))

	small, err := StartNetem(echo, NetemConfig{MTU: 1280, Seed: 1})
	require.NoError(t, err)
	defer small.Close()
	got, _ = roundTrips(t, small.Addr(), 5, 1300)
	assert.Equal(t, 0, got, "a datagram over the path MTU never arrives")
	got, _ = roundTrips(t, small.Addr(), 5, 1200)
	assert.Equal(t, 5, got)
	assert.Equal(t, int64(5), small.Stats().Oversize)
}

func TestNetemCutIsABlackholeForAWhile(t *testing.T) {
	echo := udpEcho(t)
	n, err := StartNetem(echo, NetemConfig{Seed: 1})
	require.NoError(t, err)
	defer n.Close()
	n.Cut(600 * time.Millisecond)
	got, _ := roundTrips(t, n.Addr(), 1, 100)
	assert.Equal(t, 0, got)
	time.Sleep(700 * time.Millisecond)
	got, _ = roundTrips(t, n.Addr(), 3, 100)
	assert.Equal(t, 3, got)
}

func transfer(t *testing.T, cfg NetemConfig, n int64, hook func(*Netem, int64)) (TransferResult, *Netem) {
	t.Helper()
	addr, stop, err := StartEcho(cfg.Seed)
	require.NoError(t, err)
	t.Cleanup(stop)
	nm, err := StartNetem(addr, cfg)
	require.NoError(t, err)
	t.Cleanup(nm.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var h func(int64)
	if hook != nil {
		h = func(sent int64) { hook(nm, sent) }
	}
	return RunTransfer(ctx, nm.Addr(), cfg.Seed, n, h), nm
}

// The matrix cells that fit a unit test. The full matrix (0/1/5/15/40 % by 0/50/150/300 ms) is
// run by cmd/launchpad-behaviour.
func TestNoise3CarriesATransferThroughLossAndDelay(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  NetemConfig
	}{
		{"clean", NetemConfig{Seed: 1}},
		{"1% 50 ms", NetemConfig{Loss: 0.01, Delay: 25 * time.Millisecond, Seed: 2}},
		{"5% 150 ms", NetemConfig{Loss: 0.05, Delay: 75 * time.Millisecond, Seed: 3}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := transfer(t, c.cfg, 256<<10, nil)
			require.True(t, r.Connected, r.Err)
			assert.Equal(t, int64(256<<10), r.Bytes)
			assert.Equal(t, r.Bytes, r.Echoed, "every byte comes back, once, in order")
			assert.Less(t, r.Total, 60*time.Second)
		})
	}
}

// "The session survives a change of path in at most 10 s": the client's source address changes
// halfway (NAT rebinding, Wi-Fi to mobile) and the transfer goes on.
func TestNoise3SurvivesAChangeOfSourceAddress(t *testing.T) {
	r, nm := transfer(t, NetemConfig{Seed: 4, Delay: 10 * time.Millisecond}, 512<<10, once(func(n *Netem) { n.Rebind() }))
	require.True(t, r.Connected, r.Err)
	assert.Equal(t, r.Bytes, r.Echoed)
	assert.LessOrEqual(t, r.MaxGap, 10*time.Second, "no stall longer than the 10 s of the criterion")
	_ = nm
}

// "After a break, recovery in at most 60 s": the route is cut for 3 s (a short cut for a unit
// test) and the transfer resumes by itself.
func TestNoise3RecoversAfterACut(t *testing.T) {
	r, _ := transfer(t, NetemConfig{Seed: 5}, 512<<10, once(func(n *Netem) { n.Cut(3 * time.Second) }))
	require.True(t, r.Connected, r.Err)
	assert.Equal(t, r.Bytes, r.Echoed)
	assert.LessOrEqual(t, r.MaxGap, 60*time.Second)
	assert.GreaterOrEqual(t, r.MaxGap, 2*time.Second, "the cut was felt")
}

// PMTU 1280 and 1200: no datagram of a noise3 flow before the first answer is longer than 1200
// bytes, and the default ceiling (1172) fits a path of MTU 1200 in both directions (wire.md §2.4,
// N3-FIX), so the flow connects on both.
func TestNoise3AndThePathMTU(t *testing.T) {
	ok, _ := transfer(t, NetemConfig{Seed: 6, MTU: 1500}, 32<<10, nil)
	assert.True(t, ok.Connected, ok.Err)

	for _, mtu := range []int{1280, 1200} {
		r, nm := transfer(t, NetemConfig{Seed: 6, MTU: mtu - 28}, 32<<10, nil) // the netem counts the payload
		assert.True(t, r.Connected, "MTU %d: %s", mtu, r.Err)
		assert.Zero(t, nm.Stats().Oversize, "MTU %d: nothing was lost to the size", mtu)
	}
}

func TestSoakDoesNotGrowGoroutinesOrDescriptors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r := RunSoak(ctx, SoakConfig{Duration: 6 * time.Second, ListenerRestartEvery: 5})
	assert.Greater(t, r.Cycles, 3)
	assert.Zero(t, r.Errors)
	assert.LessOrEqual(t, r.GoroutinesEnd, r.GoroutinesStart+5, "start %d, end %d", r.GoroutinesStart, r.GoroutinesEnd)
	assert.LessOrEqual(t, r.FDsEnd, r.FDsStart+5, "start %d, end %d", r.FDsStart, r.FDsEnd)
	assert.LessOrEqual(t, r.HeapEnd, r.HeapStart+64<<20)
}

func TestReportSaysWhatItDidNotCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r := RunAll(ctx, Options{Short: true})
	require.NotEmpty(t, r.Freeze)
	require.NotEmpty(t, r.Dial)
	require.NotEmpty(t, r.Net)
	assert.NotZero(t, r.Penalty.FirstBackoff)
	assert.GreaterOrEqual(t, len(r.PenaltyNever.Attempts), 5, "the never-lifted block is played too (H15)")
	assert.NotZero(t, r.Crowd.Clients)
	assert.True(t, r.ECH.Present)
	assert.NotZero(t, r.Soak.Cycles)

	all := ""
	for _, s := range r.NotChecked {
		all += s + "\n"
	}
	for _, want := range []string{"NAT", "IPv6", "DNS", "soak of two hours", "call", "20 to 5", "rotation", "shutdown"} {
		assert.Contains(t, all, want, "the report must say it did not check this")
	}
	assert.NotEmpty(t, r.Findings)
}

// On Windows a datagram written to a UDP port nobody listens on any more comes back as an ICMP
// "port unreachable", and the next read on the same socket fails with "connection reset". The netem
// used to stop forwarding for good at the first such error: a lab path that dies by itself.
func TestNetemKeepsForwardingAfterADatagramToAClosedPort(t *testing.T) {
	echo := udpEcho(t)
	em, err := StartNetem(echo, NetemConfig{Seed: 1})
	require.NoError(t, err)
	defer em.Close()

	// a client that sends and goes away before the echo comes back: the netem answers into a closed port
	gone, err := net.Dial("udp", em.Addr())
	require.NoError(t, err)
	_, err = gone.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, gone.Close())
	time.Sleep(300 * time.Millisecond)

	got, _ := roundTrips(t, em.Addr(), 10, 100)
	assert.Equal(t, 10, got, "a new client still gets its echo")
}

// The same on the side of the target: the server's port is closed for a moment, the connected socket
// of the netem hears "connection reset", and the path must still carry the traffic when it is back.
func TestNetemKeepsForwardingWhenTheTargetWasUnreachableForAMoment(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	target := pc.LocalAddr().String()
	require.NoError(t, pc.Close()) // nobody listens yet
	em, err := StartNetem(target, NetemConfig{Seed: 1})
	require.NoError(t, err)
	defer em.Close()

	c, err := net.Dial("udp", em.Addr())
	require.NoError(t, err)
	defer c.Close()
	for i := 0; i < 3; i++ { // each is forwarded into a closed port
		_, _ = c.Write([]byte("early"))
		time.Sleep(50 * time.Millisecond)
	}

	back, err := net.ListenPacket("udp", target)
	require.NoError(t, err)
	defer back.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := back.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = back.WriteTo(buf[:n], from)
		}
	}()
	buf := make([]byte, 2048)
	ok := false
	for i := 0; i < 10 && !ok; i++ {
		_, _ = c.Write([]byte("late"))
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if n, err := c.Read(buf); err == nil && string(buf[:n]) == "late" {
			ok = true
		}
	}
	assert.True(t, ok, "the same client is answered once the target is back")
}
