package network

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
)

// Anonymous mode through the user's own relay: the node connects to nothing but that relay, so no
// other node, server or peer ever sees this computer's address. Every outbound dial (TCP, QUIC,
// WebSocket, a hole punch) asks DialRefused first. Peers on the local network stay reachable, and
// a dial marked WithDirect is the user's own choice; anonymous mode never marks one.

var dialOnly atomic.Pointer[[]string]

// ErrDialRefused: anonymous mode keeps this node from connecting anywhere but its own relay.
var ErrDialRefused = errors.New("anonymous mode: this node connects only through its own relay")

// SetDialOnly lets the node connect only to hosts (host names or IP addresses, as in a host:port
// address; names are looked up too, so a dial to the address they stand for passes). nil lets it
// connect anywhere again; an empty list lets it connect nowhere.
func SetDialOnly(hosts []string) {
	if hosts == nil {
		dialOnly.Store(nil)
		return
	}
	allow := []string{}
	for _, h := range hosts {
		h = strings.ToLower(strings.Trim(strings.TrimSpace(h), "[]"))
		if h == "" {
			continue
		}
		allow = append(allow, h)
		if ips, err := net.LookupHost(h); err == nil {
			for _, ip := range ips {
				allow = append(allow, strings.ToLower(ip))
			}
		}
	}
	dialOnly.Store(&allow)
}

// DialOnly is the list of hosts the node may connect to, nil when it may connect anywhere.
func DialOnly() []string {
	if p := dialOnly.Load(); p != nil {
		return append([]string(nil), (*p)...)
	}
	return nil
}

// DialRefused reports whether a dial to addr must not happen: anonymous mode is on and addr is
// neither one of the allowed hosts nor on the local network.
func DialRefused(ctx context.Context, addr string) bool {
	p := dialOnly.Load()
	if p == nil || DirectRequested(ctx) || localNetAddr(addr) {
		return false
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	for _, a := range *p {
		if a == host {
			return false
		}
	}
	return true
}
