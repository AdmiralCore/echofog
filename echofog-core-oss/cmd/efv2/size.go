package main

import (
	"fmt"
	"strconv"
	"strings"
)

// byteSize is a flag value that accepts a plain byte count or a human suffix (64MB, 1GiB). The
// test bed passes the exact integer, an operator can type 1GB.
type byteSize int64

func (b *byteSize) String() string { return strconv.FormatInt(int64(*b), 10) }

func (b *byteSize) Set(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("empty size")
	}
	upper := strings.ToUpper(s)
	mult := int64(1)
	for _, suffix := range []struct {
		text string
		n    int64
	}{
		{"GIB", 1 << 30}, {"GB", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10},
		{"B", 1},
	} {
		if strings.HasSuffix(upper, suffix.text) {
			mult = suffix.n
			upper = strings.TrimSuffix(upper, suffix.text)
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(upper), 10, 64)
	if err != nil || n <= 0 {
		return fmt.Errorf("size %q: want bytes or a value like 64MB, 1GiB", s)
	}
	if n > (1<<62)/mult {
		return fmt.Errorf("size %q is too large", s)
	}
	*b = byteSize(n * mult)
	return nil
}
