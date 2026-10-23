package network

import (
	"context"
	"net"
	"testing"
)

// The logs name what carries a peer: TLS over TCP and WebSocket both report Protocol "tcp".
func TestLinkKindTellsWebSocketFromTCP(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = NewSmuxConn(ctx, b, nil, false) }()
	mc, err := NewSmuxConn(ctx, a, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := LinkKind(mc); got != ProtocolTCP {
		t.Fatalf("TLS over TCP: %q", got)
	}
	if got := DescribeLink(mc); got != "tcp pipe->pipe" {
		t.Fatalf("DescribeLink = %q", got)
	}
	mc.(*smuxConn).ws = true
	if got := LinkKind(mc); got != linkWS || mc.Protocol() != ProtocolTCP {
		t.Fatalf("WebSocket: kind %q, protocol %q", got, mc.Protocol())
	}
	if DescribeLink(nil) != "" {
		t.Fatal("no connection, no description")
	}
}
