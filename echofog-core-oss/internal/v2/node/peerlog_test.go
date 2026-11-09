package node_test

import (
	"bytes"
	"encoding/hex"
	"log/slog"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/handshake"
	v2node "github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/session"
	"github.com/stretchr/testify/require"
)

// TestPeerCardWarningShortensTheDevice covers the branch of peerOf that logs a card the handshake
// accepted but the raw clock rejects: the log may only carry the device prefix, never the full ID.
func TestPeerCardWarningShortensTheDevice(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var device [32]byte
	for i := range device {
		device[i] = byte(0xA0 + i*3)
	}
	full := hex.EncodeToString(device[:])
	est := &session.Established{Peer: &handshake.CardInfo{DeviceID: device, Raw: []byte("not a card")}}

	p := v2node.PeerOfForLog(logger, est)
	require.Equal(t, v2node.DeviceID(device), p.Device, "the device is still the authenticated one")
	require.Nil(t, p.Card)

	line := buf.String()
	require.Contains(t, line, "peer card no longer verifies", "the rejection branch must be covered")
	require.Contains(t, line, full[:8], "the log keeps the prefix for correlation")
	require.NotContains(t, line, full, "the full device ID must not reach the log")
}
