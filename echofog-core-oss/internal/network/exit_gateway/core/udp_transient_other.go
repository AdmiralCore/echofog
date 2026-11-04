//go:build !windows

package exit_gateway

func udpReadTransient(error) bool { return false }
