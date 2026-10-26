package exit_gateway

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockStream struct {
	readBuf          *bytes.Buffer
	writeBuf         *bytes.Buffer
	mu               sync.Mutex
	closed           bool
	closeCount       atomic.Int32
	closeWriteCalled atomic.Bool
	deadlineCalled   atomic.Bool
	udpLinkCalled    atomic.Bool
}

func newMockStream() *mockStream {
	return &mockStream{
		readBuf:  bytes.NewBuffer(nil),
		writeBuf: bytes.NewBuffer(nil),
	}
}

func (m *mockStream) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readBuf.Read(p)
}

func (m *mockStream) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeBuf.Write(p)
}

func (m *mockStream) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.closeCount.Add(1)
	return nil
}

func (m *mockStream) WriteFrame(msgType net_types.MessageType, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeBuf.Write(data)
	return nil
}

func (m *mockStream) ReadFrame() (net_types.MessageType, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data := make([]byte, m.readBuf.Len())
	n, err := m.readBuf.Read(data)
	return net_types.TypeExitConnect, data[:n], err
}

func (m *mockStream) CloseWrite() error {
	m.closeWriteCalled.Store(true)
	return nil
}

func (m *mockStream) SetDeadline(t time.Time) error {
	m.deadlineCalled.Store(true)
	return nil
}

func (m *mockStream) SetReadDeadline(t time.Time) error {
	return nil
}

func (m *mockStream) SetWriteDeadline(t time.Time) error {
	return nil
}

func (m *mockStream) udpLink(toExit bool) udpLink {
	m.udpLinkCalled.Store(true)
	return nil
}

func TestByteCounting(t *testing.T) {
	mock := newMockStream()
	mock.readBuf.WriteString("hello from exit")

	sess := NewManagedSession(1, func(ctx context.Context) (FramedStream, error) {
		return mock, nil
	}, config.DefaultFlowShapingConfig(), nil)

	st, err := sess.OpenStream(context.Background())
	require.NoError(t, err)
	defer st.Close()

	assert.Equal(t, int32(1), sess.ActiveStreams())
	assert.Equal(t, int64(1), sess.TotalStreams())

	// Test Write
	writeData := []byte("request payload data")
	n, err := st.Write(writeData)
	require.NoError(t, err)
	assert.Equal(t, len(writeData), n)
	assert.Equal(t, int64(len(writeData)), sess.BytesMoved())

	// Test Read
	readBuf := make([]byte, 64)
	n, err = st.Read(readBuf)
	require.NoError(t, err)
	assert.Equal(t, 15, n) // "hello from exit" is 15 bytes
	assert.Equal(t, int64(len(writeData)+15), sess.BytesMoved())

	// Test WriteFrame
	frameData := []byte("frame data")
	err = st.WriteFrame(net_types.TypeExitConnect, frameData)
	require.NoError(t, err)
	assert.Equal(t, int64(len(writeData)+15+len(frameData)), sess.BytesMoved())

	// Test Close decrements activeStreams
	err = st.Close()
	require.NoError(t, err)
	assert.Equal(t, int32(0), sess.ActiveStreams())
	assert.Equal(t, int64(1), sess.TotalStreams())
}

func TestRotation_ByteThreshold(t *testing.T) {
	cfg := config.FlowShapingConfig{
		MaxConnections:     3,
		MaxConcurrency:     8,
		MaxBytesMin:        100,
		MaxBytesMax:        100,
		MaxConnLifetimeMin: 10 * time.Second,
		MaxConnLifetimeMax: 10 * time.Second,
		MaxStreamsPerConn:  500,
		MinDialInterval:    10 * time.Millisecond,
	}

	var sessionDials atomic.Int32
	pool := NewPeerSessionPoolWithDialer(types.PeerID{}, cfg, func(ctx context.Context) (StreamOpener, func() error, error) {
		sessionDials.Add(1)
		return func(ctx context.Context) (FramedStream, error) {
			return newMockStream(), nil
		}, nil, nil
	})
	defer pool.Close()

	// 1. Open first stream and move bytes past threshold
	st1, err := pool.OpenStream(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int32(1), sessionDials.Load())

	// Move 150 bytes (threshold is 100)
	_, err = st1.Write(make([]byte, 150))
	require.NoError(t, err)

	active := pool.ActiveSessions()
	require.Len(t, active, 1)
	assert.True(t, active[0].ShouldRotate())

	// Wait past MinDialInterval
	time.Sleep(20 * time.Millisecond)

	// 2. Open second stream - must rotate to session 2
	st2, err := pool.OpenStream(context.Background())
	require.NoError(t, err)
	defer st2.Close()

	assert.Equal(t, int32(2), sessionDials.Load(), "should have dialed a second session due to byte rotation")

	// First session should now be draining, second session is active
	active = pool.ActiveSessions()
	require.Len(t, active, 1)
	assert.Equal(t, int64(2), active[0].ID())

	draining := pool.DrainingSessions()
	require.Len(t, draining, 1)
	assert.Equal(t, int64(1), draining[0].ID())
	assert.True(t, draining[0].IsDraining())

	// Clean up st1
	st1.Close()
}

func TestRotation_LifetimeExceeded(t *testing.T) {
	cfg := config.FlowShapingConfig{
		MaxConnections:     3,
		MaxConcurrency:     8,
		MaxBytesMin:        10 * 1024 * 1024,
		MaxBytesMax:        10 * 1024 * 1024,
		MaxConnLifetimeMin: 30 * time.Millisecond,
		MaxConnLifetimeMax: 30 * time.Millisecond,
		MaxStreamsPerConn:  500,
		MinDialInterval:    10 * time.Millisecond,
	}

	var sessionDials atomic.Int32
	pool := NewPeerSessionPoolWithDialer(types.PeerID{}, cfg, func(ctx context.Context) (StreamOpener, func() error, error) {
		sessionDials.Add(1)
		return func(ctx context.Context) (FramedStream, error) {
			return newMockStream(), nil
		}, nil, nil
	})
	defer pool.Close()

	st1, err := pool.OpenStream(context.Background())
	require.NoError(t, err)
	defer st1.Close()
	assert.Equal(t, int32(1), sessionDials.Load())

	// Wait for lifetime to expire
	time.Sleep(50 * time.Millisecond)

	active := pool.ActiveSessions()
	require.Len(t, active, 1)
	assert.True(t, active[0].ShouldRotate(), "session lifetime should be exceeded")

	// Open stream 2 - must rotate
	st2, err := pool.OpenStream(context.Background())
	require.NoError(t, err)
	defer st2.Close()

	assert.Equal(t, int32(2), sessionDials.Load(), "should have rotated to a new session")
	assert.Len(t, pool.DrainingSessions(), 1)
}

func TestRotation_GracefulDrain(t *testing.T) {
	cfg := config.FlowShapingConfig{
		MaxConnections:     3,
		MaxConcurrency:     8,
		MaxBytesMin:        50,
		MaxBytesMax:        50,
		MaxConnLifetimeMin: 10 * time.Second,
		MaxConnLifetimeMax: 10 * time.Second,
		MaxStreamsPerConn:  500,
		MinDialInterval:    5 * time.Millisecond,
	}

	var session1Closed atomic.Bool
	var sessionDials atomic.Int32

	pool := NewPeerSessionPoolWithDialer(types.PeerID{}, cfg, func(ctx context.Context) (StreamOpener, func() error, error) {
		dialNum := sessionDials.Add(1)
		closeFn := func() error {
			if dialNum == 1 {
				session1Closed.Store(true)
			}
			return nil
		}
		return func(ctx context.Context) (FramedStream, error) {
			return newMockStream(), nil
		}, closeFn, nil
	})
	pool.SetDrainTimeout(100 * time.Millisecond)
	defer pool.Close()

	// Open stream 1
	st1, err := pool.OpenStream(context.Background())
	require.NoError(t, err)

	// Exceed byte limit on stream 1
	_, err = st1.Write(make([]byte, 100))
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond)

	// Open stream 2 -> triggers rotation
	st2, err := pool.OpenStream(context.Background())
	require.NoError(t, err)
	defer st2.Close()

	// Session 1 is draining, but st1 is still open!
	draining := pool.DrainingSessions()
	require.Len(t, draining, 1)
	assert.Equal(t, int32(1), draining[0].ActiveStreams(), "session 1 should still have 1 active stream")
	assert.False(t, session1Closed.Load(), "session 1 closeFn must not be called while stream is active")

	// Stream 1 can still do work
	_, err = st1.Write([]byte("still working"))
	require.NoError(t, err)

	// Now close stream 1
	err = st1.Close()
	require.NoError(t, err)

	// Wait for drain to finish
	require.Eventually(t, func() bool {
		return session1Closed.Load() && len(pool.DrainingSessions()) == 0
	}, 200*time.Millisecond, 10*time.Millisecond, "session 1 should close and be removed from draining list once stream closes")
}

func TestPacingRateLimiter_AndThroughputContinuity(t *testing.T) {
	// Test requirement:
	// "Ensure that if bytes exceed threshold before minDialInterval (e.g. on gigabit line),
	// we continue on the current session rather than blocking traffic until minDialInterval passes."
	cfg := config.FlowShapingConfig{
		MaxConnections:     3,
		MaxConcurrency:     8,
		MaxBytesMin:        100,
		MaxBytesMax:        100,
		MaxConnLifetimeMin: 10 * time.Second,
		MaxConnLifetimeMax: 10 * time.Second,
		MaxStreamsPerConn:  500,
		MinDialInterval:    500 * time.Millisecond, // long dial interval
	}

	var sessionDials atomic.Int32
	pool := NewPeerSessionPoolWithDialer(types.PeerID{}, cfg, func(ctx context.Context) (StreamOpener, func() error, error) {
		sessionDials.Add(1)
		return func(ctx context.Context) (FramedStream, error) {
			return newMockStream(), nil
		}, nil, nil
	})
	defer pool.Close()

	// Open stream 1 and exceed byte limit immediately
	st1, err := pool.OpenStream(context.Background())
	require.NoError(t, err)
	defer st1.Close()

	_, err = st1.Write(make([]byte, 200)) // exceeds 100 byte limit
	require.NoError(t, err)

	active := pool.ActiveSessions()
	require.Len(t, active, 1)
	assert.True(t, active[0].ShouldRotate(), "session should want to rotate")

	// Immediately request stream 2 (long before 500ms pacing interval)
	start := time.Now()
	st2, err := pool.OpenStream(context.Background())
	duration := time.Since(start)

	require.NoError(t, err)
	defer st2.Close()

	// Must NOT block for 500ms! Should return in under 50ms
	assert.Less(t, duration, 100*time.Millisecond, "must not block traffic waiting for minDialInterval")
	assert.Equal(t, int32(1), sessionDials.Load(), "must continue on current session due to dial pacing")

	// After minDialInterval has elapsed, next stream should rotate
	time.Sleep(510 * time.Millisecond)

	st3, err := pool.OpenStream(context.Background())
	require.NoError(t, err)
	defer st3.Close()

	assert.Equal(t, int32(2), sessionDials.Load(), "should rotate now that pacing interval has elapsed")
}

func TestThunderingHerdProtection(t *testing.T) {
	cfg := config.FlowShapingConfig{
		MaxConnections:     3,
		MaxConcurrency:     8,
		MaxBytesMin:        10 * 1024 * 1024,
		MaxBytesMax:        10 * 1024 * 1024,
		MaxConnLifetimeMin: 10 * time.Second,
		MaxConnLifetimeMax: 10 * time.Second,
		MaxStreamsPerConn:  500,
		MinDialInterval:    20 * time.Millisecond,
	}

	var dials atomic.Int32
	pool := NewPeerSessionPoolWithDialer(types.PeerID{}, cfg, func(ctx context.Context) (StreamOpener, func() error, error) {
		dials.Add(1)
		time.Sleep(10 * time.Millisecond) // simulate dial latency
		return func(ctx context.Context) (FramedStream, error) {
			return newMockStream(), nil
		}, nil, nil
	})
	defer pool.Close()

	// 20 concurrent goroutines calling OpenStream simultaneously
	const numGoroutines = 20
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	streams := make([]FramedStream, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			st, err := pool.OpenStream(context.Background())
			if err == nil {
				streams[idx] = st
			}
		}(i)
	}

	wg.Wait()

	// Verify all streams succeeded
	for _, st := range streams {
		require.NotNil(t, st)
		_ = st.Close()
	}

	// Dials must be bounded (<= MaxConnections, which is 3), definitely NOT 20 parallel dials!
	assert.LessOrEqual(t, dials.Load(), int32(cfg.MaxConnections), "thundering herd must not cause parallel dials")
}

func TestCountingStream_InterfacePassThrough(t *testing.T) {
	mock := newMockStream()
	sess := NewManagedSession(1, func(ctx context.Context) (FramedStream, error) {
		return mock, nil
	}, config.DefaultFlowShapingConfig(), nil)

	st, err := sess.OpenStream(context.Background())
	require.NoError(t, err)

	// CloseWrite
	cw, ok := st.(interface{ CloseWrite() error })
	require.True(t, ok)
	err = cw.CloseWrite()
	require.NoError(t, err)
	assert.True(t, mock.closeWriteCalled.Load())

	// SetDeadline
	dl, ok := st.(interface{ SetDeadline(time.Time) error })
	require.True(t, ok)
	err = dl.SetDeadline(time.Now())
	require.NoError(t, err)
	assert.True(t, mock.deadlineCalled.Load())

	// udpLink
	ds, ok := st.(datagramStream)
	require.True(t, ok)
	_ = ds.udpLink(true)
	assert.True(t, mock.udpLinkCalled.Load())

	// Double Close protection
	assert.Equal(t, int32(1), sess.ActiveStreams())
	err = st.Close()
	require.NoError(t, err)
	assert.Equal(t, int32(0), sess.ActiveStreams())

	// Second Close should NOT decrement activeStreams below 0
	err = st.Close()
	require.NoError(t, err)
	assert.Equal(t, int32(0), sess.ActiveStreams())
	assert.Equal(t, int32(1), mock.closeCount.Load(), "underlying stream should only be closed once")
}

func TestSwarmOpenerIntegration(t *testing.T) {
	ResetSwarmPools()
	defer ResetSwarmPools()

	peerID := types.PeerID{1, 2, 3}
	cfg := config.FlowShapingConfig{
		MaxConnections:     2,
		MaxConcurrency:     4,
		MaxBytesMin:        1000,
		MaxBytesMax:        1000,
		MaxConnLifetimeMin: 5 * time.Second,
		MaxConnLifetimeMax: 5 * time.Second,
		MaxStreamsPerConn:  100,
		MinDialInterval:    10 * time.Millisecond,
	}

	// Nil swarm returns errNoStreamOpener
	opener := SwarmOpenerWithConfig(nil, peerID, cfg)
	_, err := opener(context.Background())
	assert.ErrorIs(t, err, errNoStreamOpener)
}
