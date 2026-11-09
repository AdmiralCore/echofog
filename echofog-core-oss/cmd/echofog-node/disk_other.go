//go:build !linux && !windows

package main

import "errors"

// freeBytes: the free space is not measured on this platform; the doctor says so.
func freeBytes(string) (uint64, error) { return 0, errors.New("free space is not measured here") }
