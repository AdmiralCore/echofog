//go:build !windows

package core

func udpReadTransient(error) bool { return false }
