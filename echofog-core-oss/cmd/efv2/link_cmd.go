package main

import (
	"errors"
	"io"
	"time"
)

var errLinkNotImplemented = errors.New("link: not implemented")

func cmdLink([]string) error { return errLinkNotImplemented }

func linkOpen(io.Writer, string, time.Time) error { return errLinkNotImplemented }
