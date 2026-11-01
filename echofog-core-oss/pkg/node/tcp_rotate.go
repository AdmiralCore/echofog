package node

import "github.com/echofog/echofog-core/internal/config"

// applyTCPRotate tells the TCP transport how the config wants link rotation (the answer to the
// TSPU freeze, see internal/network/rotconn.go): "on" rotates every link this node dials, "off"
// never, and "auto" leaves the transport to what it learns from a frozen link (the app adds the
// country: see NodeApi.applyTCPRotateAuto).
func (n *Node) applyTCPRotate() {
	if n.Net == nil || n.Cfg == nil {
		return
	}
	a := n.Cfg.AntiDPI
	switch {
	case a.TCPRotate == config.TCPRotateOff:
		n.Net.SetTCPRotateOff(true)
	case a.TCPRotate == config.TCPRotateOn || a.MicroChunkTCP:
		n.Net.SetTCPRotate(func(string) bool { return true })
	}
}
