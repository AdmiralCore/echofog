package quic

import (
	"fmt"
	"time"

	"github.com/apernet/quic-go/internal/protocol"
	"github.com/apernet/quic-go/quicvarint"
)

// Clone clones a Config.
func (c *Config) Clone() *Config {
	copy := *c
	return &copy
}

func (c *Config) handshakeTimeout() time.Duration {
	return 2 * c.HandshakeIdleTimeout
}

// maxUDPPayloadSize is the max_udp_payload_size to advertise: the configured
// value, or quic-go's historical 1452.
func (c *Config) maxUDPPayloadSize() protocol.ByteCount {
	if c.MaxUDPPayloadSize > 0 {
		return protocol.ByteCount(c.MaxUDPPayloadSize)
	}
	return protocol.MaxPacketBufferSize
}

func (c *Config) maxRetryTokenAge() time.Duration {
	return c.handshakeTimeout()
}

func validateConfig(config *Config) error {
	if config == nil {
		return nil
	}
	const maxStreams = 1 << 60
	if config.MaxIncomingStreams > maxStreams {
		config.MaxIncomingStreams = maxStreams
	}
	if config.MaxIncomingUniStreams > maxStreams {
		config.MaxIncomingUniStreams = maxStreams
	}
	if config.MaxStreamReceiveWindow > quicvarint.Max {
		config.MaxStreamReceiveWindow = quicvarint.Max
	}
	if config.MaxConnectionReceiveWindow > quicvarint.Max {
		config.MaxConnectionReceiveWindow = quicvarint.Max
	}
	// AllowSmallInitial (EchoFog noise3) lowers the floors: the outer path of a noise3 flow may
	// carry less than 1200 bytes of inner datagram.
	minPacket := uint16(protocol.MinInitialPacketSize)
	if config.AllowSmallInitial {
		minPacket = 800
	}
	if config.InitialPacketSize > 0 && config.InitialPacketSize < minPacket {
		config.InitialPacketSize = minPacket
	}
	if config.InitialPacketSize > protocol.MaxPacketBufferSize {
		config.InitialPacketSize = protocol.MaxPacketBufferSize
	}
	if config.InitialFlightSize != 0 && (config.InitialFlightSize < 200 || !config.AllowSmallInitial) {
		return fmt.Errorf("invalid initial flight size: %d (minimum 200, needs AllowSmallInitial)", config.InitialFlightSize)
	}
	if config.MaxUDPPayloadSize != 0 && config.MaxUDPPayloadSize < minPacket {
		return fmt.Errorf("invalid max_udp_payload_size: %d (minimum %d)", config.MaxUDPPayloadSize, minPacket)
	}
	// check that all QUIC versions are actually supported
	for _, v := range config.Versions {
		if !protocol.IsValidVersion(v) {
			return fmt.Errorf("invalid QUIC version: %s", v)
		}
	}
	return nil
}

// populateConfig populates fields in the quic.Config with their default values, if none are set
// it may be called with nil
func populateConfig(config *Config) *Config {
	if config == nil {
		config = &Config{}
	}
	versions := config.Versions
	if len(versions) == 0 {
		versions = protocol.SupportedVersions
	}
	handshakeIdleTimeout := protocol.DefaultHandshakeIdleTimeout
	if config.HandshakeIdleTimeout != 0 {
		handshakeIdleTimeout = config.HandshakeIdleTimeout
	}
	idleTimeout := protocol.DefaultIdleTimeout
	if config.MaxIdleTimeout != 0 {
		idleTimeout = config.MaxIdleTimeout
	}
	initialStreamReceiveWindow := config.InitialStreamReceiveWindow
	if initialStreamReceiveWindow == 0 {
		initialStreamReceiveWindow = protocol.DefaultInitialMaxStreamData
	}
	maxStreamReceiveWindow := config.MaxStreamReceiveWindow
	if maxStreamReceiveWindow == 0 {
		maxStreamReceiveWindow = protocol.DefaultMaxReceiveStreamFlowControlWindow
	}
	initialConnectionReceiveWindow := config.InitialConnectionReceiveWindow
	if initialConnectionReceiveWindow == 0 {
		initialConnectionReceiveWindow = protocol.DefaultInitialMaxData
	}
	maxConnectionReceiveWindow := config.MaxConnectionReceiveWindow
	if maxConnectionReceiveWindow == 0 {
		maxConnectionReceiveWindow = protocol.DefaultMaxReceiveConnectionFlowControlWindow
	}
	maxIncomingStreams := config.MaxIncomingStreams
	if maxIncomingStreams == 0 {
		maxIncomingStreams = protocol.DefaultMaxIncomingStreams
	} else if maxIncomingStreams < 0 {
		maxIncomingStreams = 0
	}
	maxIncomingUniStreams := config.MaxIncomingUniStreams
	if maxIncomingUniStreams == 0 {
		maxIncomingUniStreams = protocol.DefaultMaxIncomingUniStreams
	} else if maxIncomingUniStreams < 0 {
		maxIncomingUniStreams = 0
	}
	initialPacketSize := config.InitialPacketSize
	if initialPacketSize == 0 {
		initialPacketSize = protocol.InitialPacketSize
	}
	enableDatagrams := config.EnableDatagrams
	omitMaxDatagramFrameSize := config.OmitMaxDatagramFrameSize
	if config.ChromeParrot {
		// Chrome always advertises DATAGRAM support, so enable it and never omit
		// the transport parameter; leaving it out would be one parameter short of
		// Chrome's set.
		enableDatagrams = true
		omitMaxDatagramFrameSize = false
		// Chrome pins these, so anything the caller asked for is overridden.
		idleTimeout = chromeMaxIdleTimeout
		initialStreamReceiveWindow = chromeInitialMaxStreamData
		initialConnectionReceiveWindow = chromeInitialMaxData
		maxIncomingStreams = chromeMaxIncomingStreams
		maxIncomingUniStreams = chromeMaxIncomingUniStreams
		// Chrome sends 1250; a caller may ask for less (1232 towards an IPv6 address, which must
		// fit PMTU 1280: 1232 + 48), never for more.
		if config.InitialPacketSize > 0 && config.InitialPacketSize < chromeInitialPacketSize {
			initialPacketSize = config.InitialPacketSize
		} else {
			initialPacketSize = chromeInitialPacketSize
		}
		// The auto-tuning ceilings must not sit below the starting windows.
		maxStreamReceiveWindow = max(maxStreamReceiveWindow, initialStreamReceiveWindow)
		maxConnectionReceiveWindow = max(maxConnectionReceiveWindow, initialConnectionReceiveWindow)
	}

	return &Config{
		GetConfigForClient:               config.GetConfigForClient,
		Versions:                         versions,
		HandshakeIdleTimeout:             handshakeIdleTimeout,
		MaxIdleTimeout:                   idleTimeout,
		KeepAlivePeriod:                  config.KeepAlivePeriod,
		KeepAliveJitter:                  config.KeepAliveJitter,
		InitialStreamReceiveWindow:       initialStreamReceiveWindow,
		MaxStreamReceiveWindow:           maxStreamReceiveWindow,
		InitialConnectionReceiveWindow:   initialConnectionReceiveWindow,
		MaxConnectionReceiveWindow:       maxConnectionReceiveWindow,
		AllowConnectionWindowIncrease:    config.AllowConnectionWindowIncrease,
		MaxIncomingStreams:               maxIncomingStreams,
		MaxIncomingUniStreams:            maxIncomingUniStreams,
		TokenStore:                       config.TokenStore,
		EnableDatagrams:                  enableDatagrams,
		OmitMaxDatagramFrameSize:         omitMaxDatagramFrameSize,
		AssumePeerMaxDatagramFrameSize:   config.AssumePeerMaxDatagramFrameSize,
		InitialPacketSize:                initialPacketSize,
		AllowSmallInitial:                config.AllowSmallInitial,
		InitialFlightSize:                config.InitialFlightSize,
		MaxUDPPayloadSize:                config.MaxUDPPayloadSize,
		DisablePathMTUDiscovery:          config.DisablePathMTUDiscovery,
		EnableStreamResetPartialDelivery: config.EnableStreamResetPartialDelivery,
		Allow0RTT:                        config.Allow0RTT,
		Tracer:                           config.Tracer,
		MaxDatagramFrameSize:             config.MaxDatagramFrameSize,
		DisablePathManager:               config.DisablePathManager,
		ChromeParrot:                     config.ChromeParrot,
		ClientHelloHook:                  config.ClientHelloHook,
	}
}
