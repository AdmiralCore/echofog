package network

import "testing"

func TestAddrPolicyShareable(t *testing.T) {
	public := AddrPolicy{}
	cases := []struct {
		addr string
		want bool
	}{
		{"34.158.189.116:443", true},
		{"203.0.113.7:4242", true},
		{"[2001:db8::1]:443", true},
		{"seed.example.org:443", true},
		{"wss://cdn.example.org/ws", true},
		{"10.0.0.5:4242", false},
		{"192.168.1.20:4242", false},
		{"172.20.0.3:4242", false},
		{"100.64.3.4:4242", false}, // CGNAT
		{"127.0.0.1:4242", false},
		{"[::1]:4242", false},
		{"[fd00::1]:4242", false},
		{"169.254.1.1:4242", false},
		{"0.0.0.0:4242", false},
		{"localhost:4242", false},
		{"ws://192.168.1.1/ws", false},
		{"34.158.189.116:0", false},
		{"34.158.189.116", false},
		{"", false},
	}
	for _, c := range cases {
		if got := public.Shareable(c.addr); got != c.want {
			t.Errorf("Shareable(%q) = %v, want %v", c.addr, got, c.want)
		}
	}

	loop := PolicyFor("127.0.0.1:0", false)
	if !loop.Shareable("127.0.0.1:4242") || loop.Shareable("10.0.0.5:4242") {
		t.Error("a node on loopback (a test on one machine) takes loopback addresses, still not private ones")
	}
	if PolicyFor(":4242", false).AllowLoopback {
		t.Error("a node listening on every interface is not a loopback test setup")
	}
	if !PolicyFor(":4242", true).Shareable("172.20.0.3:4242") {
		t.Error("allow_private_addrs lets private addresses through (a Docker or LAN network)")
	}
}

func TestInboundListenAddr(t *testing.T) {
	addr, ok := inboundListenAddr("34.158.189.116:4242", 443)
	if !ok || addr != "34.158.189.116:443" {
		t.Fatalf("got %q %v: an inbound peer is reached at its source IP and the port it announced", addr, ok)
	}
	if _, ok := inboundListenAddr("34.158.189.116:4242", 0); ok {
		t.Fatal("a peer that announced no port must not be dialable at its source port")
	}
	if addr, ok := inboundListenAddr("[2001:db8::1]:5000", 4242); !ok || addr != "[2001:db8::1]:4242" {
		t.Fatalf("IPv6: got %q %v", addr, ok)
	}
}
