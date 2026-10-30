package names

import (
	"errors"
	"strings"
)

// Network is the naming system a name lives in, told by its ending.
type Network string

const (
	TON      Network = "ton"  // alice.ton and its subdomains (TON DNS)
	Telegram Network = "t.me" // alice.t.me: a Telegram username held as an NFT on TON, same resolver
	Solana   Network = "sol"  // alice.sol (Solana Name Service): recognised, not resolved yet
)

// ErrNotName: the text is not a name this app looks up.
var ErrNotName = errors.New("not a .ton, .t.me or .sol name")

// Names are ASCII only, so that two names that look the same are the same name (no homoglyphs).
func labelOK(l string, min, max int, extra string) bool {
	if len(l) < min || len(l) > max || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || strings.IndexByte(extra, c) >= 0) {
			return false
		}
	}
	return true
}

// Normalize lowercases a name, drops a trailing dot and checks it: alice.ton, sub.alice.ton,
// alice.t.me, alice.sol.
func Normalize(text string) (string, Network, error) {
	n := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(text)), ".")
	switch {
	case strings.HasSuffix(n, ".t.me"):
		user := strings.TrimSuffix(n, ".t.me")
		if !labelOK(user, 4, 32, "_") || strings.Contains(user, "-") {
			return "", "", ErrNotName
		}
		return n, Telegram, nil
	case strings.HasSuffix(n, ".ton"):
		labels := strings.Split(strings.TrimSuffix(n, ".ton"), ".")
		if len(n) > 126 {
			return "", "", ErrNotName
		}
		// a second-level .ton name has 4 to 126 characters; a subdomain's resolver sets its own rules
		if !labelOK(labels[len(labels)-1], 4, 126, "") {
			return "", "", ErrNotName
		}
		for _, l := range labels[:len(labels)-1] {
			if !labelOK(l, 1, 63, "") {
				return "", "", ErrNotName
			}
		}
		return n, TON, nil
	case strings.HasSuffix(n, ".sol"):
		for _, l := range strings.Split(strings.TrimSuffix(n, ".sol"), ".") {
			if !labelOK(l, 1, 63, "_") {
				return "", "", ErrNotName
			}
		}
		return n, Solana, nil
	}
	return "", "", ErrNotName
}

// IsName reports whether text is a name to look up (for the search box and the open-link dialog).
func IsName(text string) bool {
	_, _, err := Normalize(text)
	return err == nil
}
