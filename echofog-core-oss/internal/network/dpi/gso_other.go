//go:build !linux

package dpi

// gsoSegment: GSO exists only on Linux; quic-go never asks for it elsewhere.
func gsoSegment(oob []byte) (size, at int) { return 0, -1 }

func setGSOSegment(oob []byte, at, size int) {}

func dropGSO(dst, oob []byte) []byte { return append(dst, oob...) }
