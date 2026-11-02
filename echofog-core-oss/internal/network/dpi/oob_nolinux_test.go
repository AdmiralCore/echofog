//go:build !linux

package dpi

func ecnMarked(oob []byte) bool { return false }
