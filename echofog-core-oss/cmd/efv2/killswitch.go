package main

import (
	"errors"
	"io"

	"github.com/echofog/echofog-core/internal/v2/tun/platform"
)

var (
	killswitchRemove = platform.RemoveAll
	killswitchStands = platform.FiltersStand
)

func cmdKillswitch(args []string, out io.Writer) error { return errors.New("not implemented") }
