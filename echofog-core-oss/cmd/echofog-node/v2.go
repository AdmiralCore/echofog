package main

import "errors"

// protocolGuard refuses the v1 run path under ECHOFOG_PROTOCOL=2.
func protocolGuard(cmd string, env func(string) string) error { return errors.New("not implemented") }
