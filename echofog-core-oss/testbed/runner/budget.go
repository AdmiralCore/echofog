package main

import (
	"fmt"
	"strconv"
	"strings"
)

// maxPcapBudget is the hard cap from 03-launchpad-v2.md §7: one run may not write more than 2 GiB.
const maxPcapBudget int64 = 2 << 30

// parseByteSize reads "2GB", "512MB", "1024" and the like.
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "GB") || strings.HasSuffix(s, "GIB"):
		mult = 1 << 30
		s = strings.TrimSuffix(strings.TrimSuffix(s, "GB"), "GIB")
	case strings.HasSuffix(s, "MB") || strings.HasSuffix(s, "MIB"):
		mult = 1 << 20
		s = strings.TrimSuffix(strings.TrimSuffix(s, "MB"), "MIB")
	case strings.HasSuffix(s, "KB") || strings.HasSuffix(s, "KIB"):
		mult = 1 << 10
		s = strings.TrimSuffix(strings.TrimSuffix(s, "KB"), "KIB")
	case strings.HasSuffix(s, "TB") || strings.HasSuffix(s, "TIB"):
		return 0, fmt.Errorf("unsupported size %q (the cap is 2 GiB)", s)
	}
	if s == "" || strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("invalid size")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size")
	}
	out := int64(v * float64(mult))
	if out <= 0 {
		return 0, fmt.Errorf("invalid size")
	}
	return out, nil
}

// validatePcapBudget refuses a run without a budget and refuses one above the 2 GiB cap.
func validatePcapBudget(n int64) error {
	if n <= 0 {
		return fmt.Errorf("no pcap budget: the runner does not start without one (-pcap-budget)")
	}
	if n > maxPcapBudget {
		return fmt.Errorf("pcap budget %s is above the %s per-run cap", humanBytes(n), humanBytes(maxPcapBudget))
	}
	return nil
}
