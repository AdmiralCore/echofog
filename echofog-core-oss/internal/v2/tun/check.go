package tun

import "errors"

// ErrBadRules is a rule set a driver refuses to install.
var ErrBadRules = errors.New("tun: the rules do not keep the computer closed")

// MaxRules bounds a rule set. BuildRules makes about 40 rules plus four per endpoint.
const MaxRules = 1024

// CheckRules is the last line before a driver touches the firewall: it refuses a rule set that does
// not close the computer, whoever built it (a driver also receives its rules from another process).
// The set must be persistent, named, bounded, and must block every probe packet that has no business
// leaving the physical interface: public addresses of both families, every direction, TCP, UDP and
// ICMP, and the DNS ports even towards a LAN address.
func CheckRules(rules []Rule) error {
	return errors.New("not implemented")
}
