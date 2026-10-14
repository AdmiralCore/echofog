package bwprobe

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type link struct {
	handlers map[types.PeerID]func([]byte, types.PeerID) error
	self     types.PeerID
	peer     *link
	delay    time.Duration
}

func (l *link) SendRawFrame(to types.PeerID, _ net_types.MessageType, data []byte) error {
	if l.peer == nil || to != l.peer.self {
		return assert.AnError
	}
	cp := append([]byte(nil), data...)
	if l.delay > 0 && len(cp) > headerSize {
		time.Sleep(l.delay)
	}
	h := l.peer.handlers[l.peer.self]
	return h(cp, l.self)
}

func (l *link) RegisterFastFrameHandler(_ net_types.MessageType, h func([]byte, types.PeerID) error) {
	l.handlers[l.self] = h
}

func pair(delay time.Duration) (*Prober, *Prober, types.PeerID, types.PeerID) {
	var a, b types.PeerID
	a[0], b[0] = 1, 2
	la := &link{handlers: map[types.PeerID]func([]byte, types.PeerID) error{}, self: a, delay: delay}
	lb := &link{handlers: map[types.PeerID]func([]byte, types.PeerID) error{}, self: b}
	la.peer, lb.peer = lb, la
	return New(la), New(lb), a, b
}

func TestMeasureReportsPeerObservedRate(t *testing.T) {
	pa, _, _, b := pair(2 * time.Millisecond)
	kbps, err := pa.Measure(context.Background(), b)
	require.NoError(t, err)
	// 16 KiB per ~2ms is about 65 Mbit/s; accept a wide band, only the order of magnitude matters
	assert.Greater(t, kbps, 5_000)
	assert.Less(t, kbps, 2_000_000)
}

func TestMeasureBestAppliesHeadroomAndHandlesNoPeers(t *testing.T) {
	pa, _, _, b := pair(0)
	_, err := pa.MeasureBest(context.Background(), nil, 3)
	assert.ErrorIs(t, err, ErrNoPeers)

	raw, err := pa.Measure(context.Background(), b)
	require.NoError(t, err)
	best, err := pa.MeasureBest(context.Background(), []types.PeerID{b}, 3)
	require.NoError(t, err)
	assert.Greater(t, best, 0)
	assert.LessOrEqual(t, best, raw*2)
}

func TestMeasureDownloadSeenByRequester(t *testing.T) {
	pa, _, _, b := pair(0)
	kbps, err := pa.MeasureDownload(context.Background(), b)
	require.NoError(t, err)
	assert.Greater(t, kbps, 1_000)

	_, err = pa.MeasureDownload(context.Background(), b)
	assert.Error(t, err, "a second request inside the cooldown must be ignored")
}
