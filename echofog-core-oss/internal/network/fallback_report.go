package network

// PrefersTCPHere reports whether TCP goes first on the current network: the transport is set to TCP,
// or QUIC did not get through here recently (UDP blocked or throttled). For a connection report.
func (f *FallbackTransport) PrefersTCPHere() bool {
	return f.TCP != nil && (f.mode == ModeTCP || f.envPrefersTCP())
}
