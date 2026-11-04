package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProtocolDefaultsToV1(t *testing.T) {
	cfg := DefaultConfig("")
	require.Equal(t, ProtocolV1, cfg.Protocol, "the default is the v1 protocol")
	require.Equal(t, ProtocolV1, cfg.ProtocolOrDefault())
	require.False(t, cfg.IsV2())
}

func TestProtocolOrDefaultRejectsUnknownValues(t *testing.T) {
	cfg := DefaultConfig("")
	cfg.Protocol = 7
	require.Equal(t, ProtocolV1, cfg.ProtocolOrDefault(), "an unknown value means v1, never a half mode")
	require.False(t, cfg.IsV2())
}

func TestProtocolJSONRoundTrip(t *testing.T) {
	cfg := DefaultConfig("p")
	cfg.Protocol = ProtocolV2
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)

	var back AppConfig
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, ProtocolV2, back.Protocol)
	require.True(t, back.IsV2())
}
