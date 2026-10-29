package censor

import "strconv"

// The TSPU freeze (Russia): a TCP connection from inside the border to a foreign hosting address
// that the censor finds suspicious works for about 16 KB of the server's answer and then goes
// silent, with no reset, until the client gives up. QUIC (UDP) is not subject to it. The rule below
// reproduces that at a lab router; FreezeExposed says, from a capture, which flows a router with
// the rule would have cut.

// FreezeBytes is the server-to-client volume a flow gets before it freezes.
const FreezeBytes = 16 * 1024

// FreezeRule returns the iptables arguments, for a router's FORWARD chain (after -I FORWARD 1 or
// -D FORWARD), that cut what the servers in the address range [from, to] send on a TCP connection
// once the connection carried limit bytes of it. The site every browser visits (web) is not in the
// range: the censor leaves ordinary sites alone, and the control must keep working.
func FreezeRule(from, to string, limit int) []string {
	return []string{"-p", "tcp", "-m", "iprange", "--src-range", from + "-" + to,
		"-m", "connbytes", "--connbytes", strconv.Itoa(limit) + ":", "--connbytes-dir", "reply",
		"--connbytes-mode", "bytes", "-j", "DROP"}
}

// FreezeExposed: the flow is TCP and its server sent at least FreezeBytes of payload, so a router
// with the freeze rule would have cut it. A UDP flow (QUIC, noise) is never exposed.
func (f Flow) FreezeExposed() bool { return f.Proto == "tcp" && f.PayDn >= FreezeBytes }
