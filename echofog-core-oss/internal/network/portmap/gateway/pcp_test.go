package gateway

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// fakePCP answers PCP on loopback: ANNOUNCE, and MAP with external address 203.0.113.5. Ports in
// taken are refused as "no resources" when suggested; then it gives 40000.
type fakePCP struct {
	conn   *net.UDPConn
	mu     sync.Mutex // guards nonces and maps, read by the test
	natpmp bool       // answer like a router that only knows NAT-PMP
	taken  map[int]bool
	nonces map[[12]byte]int // nonce -> how many MAPs carried it
	maps   int
}

func startFakePCP(t *testing.T, natpmp bool, taken ...int) (*fakePCP, *net.UDPAddr) {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePCP{conn: c, natpmp: natpmp, taken: map[int]bool{}, nonces: map[[12]byte]int{}}
	for _, p := range taken {
		f.taken[p] = true
	}
	t.Cleanup(func() { c.Close() })
	go f.serve()
	return f, c.LocalAddr().(*net.UDPAddr)
}

func (f *fakePCP) serve() {
	buf := make([]byte, 1100)
	for {
		n, from, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		req := buf[:n]
		if f.natpmp {
			// NAT-PMP: version 0, the opcode with the response bit, result 1 (unsupported version)
			_, _ = f.conn.WriteToUDP([]byte{0, req[1] | 0x80, 0, 1, 0, 0, 0, 0}, from)
			continue
		}
		resp := make([]byte, pcpHeaderLen, pcpHeaderLen+pcpMapLen)
		resp[0], resp[1] = pcpVersion, req[1]|pcpResponse
		copy(resp[4:8], req[4:8])
		if req[1] == opMap {
			d := append([]byte(nil), req[pcpHeaderLen:pcpHeaderLen+pcpMapLen]...)
			f.mu.Lock()
			f.maps++
			f.nonces[[12]byte(d[0:12])]++
			f.mu.Unlock()
			want := int(binary.BigEndian.Uint16(d[18:20]))
			if want != 0 && f.taken[want] {
				resp[3] = pcpCannotProvide
				_, _ = f.conn.WriteToUDP(resp, from)
				continue
			}
			ext := want
			if ext == 0 {
				ext = 40000
			}
			binary.BigEndian.PutUint16(d[18:20], uint16(ext))
			copy(d[20:36], net.IPv4(203, 0, 113, 5).To16())
			resp = append(resp, d...)
		}
		_, _ = f.conn.WriteToUDP(resp, from)
	}
}

func TestPCPMapsAndRenewsAPort(t *testing.T) {
	f, addr := startFakePCP(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	n, err := discoverPCPAt(ctx, addr)
	if err != nil {
		t.Fatalf("a PCP server was not found: %v", err)
	}
	if _, err := n.GetExternalAddress(); err == nil {
		t.Fatal("an external address before any mapping")
	}
	ext, err := n.AddPortMapping(ctx, "udp", 4242, "echofog", time.Minute)
	if err != nil || ext != 4242 {
		t.Fatalf("mapping: port %d, err %v; want the same port outside", ext, err)
	}
	ip, err := n.GetExternalAddress()
	if err != nil || !ip.Equal(net.IPv4(203, 0, 113, 5)) {
		t.Fatalf("external address %v, err %v", ip, err)
	}
	// renewing reuses the nonce, so the gateway keeps the same mapping
	if _, err := n.AddPortMapping(ctx, "udp", 4242, "echofog", time.Minute); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.nonces) != 1 || f.maps != 2 {
		t.Fatalf("%d nonces over %d MAPs; a renewal must reuse the nonce", len(f.nonces), f.maps)
	}
}

func TestPCPTakesAnotherPortWhenTheSameIsTaken(t *testing.T) {
	_, addr := startFakePCP(t, false, 4243)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := discoverPCPAt(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	ext, err := n.AddPortMapping(ctx, "tcp", 4243, "echofog", time.Minute)
	if err != nil || ext != 40000 {
		t.Fatalf("got port %d, err %v; want the one the server chose", ext, err)
	}
}

// A router that only speaks NAT-PMP says so at once, so discovery moves on to NAT-PMP quickly.
func TestPCPGivesWayToNATPMP(t *testing.T) {
	_, addr := startFakePCP(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := discoverPCPAt(ctx, addr)
	if pe, ok := err.(pcpError); !ok || pe.code != pcpUnsuppVersion {
		t.Fatalf("got %v, want unsupported version", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the NAT-PMP answer was not taken as final")
	}
}
