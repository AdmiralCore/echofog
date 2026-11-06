package main

import (
	"errors"
	"strings"
)

// protocolGuard refuses the v1 run path under ECHOFOG_PROTOCOL=2. T11.1a made that variable start
// the process without a network, which is meaningless for a server; the v2 server is its own
// command now.
func protocolGuard(cmd string, env func(string) string) error {
	if cmd == "run" && strings.TrimSpace(env("ECHOFOG_PROTOCOL")) == "2" {
		return errors.New("ECHOFOG_PROTOCOL=2: the v1 node does not run in protocol 2; use `echofog-node v2 run --data DIR` (see `echofog-node v2 help`)")
	}
	return nil
}
