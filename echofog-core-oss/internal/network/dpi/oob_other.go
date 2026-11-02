//go:build !linux && !darwin && !freebsd

package dpi

// StripECN: quic-go writes no control messages on this platform.
func StripECN(dst, oob []byte) []byte { return append(dst, oob...) }

// HasECNMark: quic-go writes no control messages on this platform.
func HasECNMark(oob []byte) bool { return false }
