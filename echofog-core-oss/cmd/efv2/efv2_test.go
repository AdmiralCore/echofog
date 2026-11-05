package main

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

func TestParseListen(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		cover   string
		want    []listenSpec
		wantErr string
	}{
		{name: "empty is a client only node", spec: ""},
		{name: "wildcard binds and advertises loopback", spec: "noise3=:4433",
			want: []listenSpec{{Suite: objects.SuiteNoise3, Bind: ":4433", AdvertiseHost: "127.0.0.1", Port: 4433}}},
		{name: "unspecified IPv4", spec: "noise3=0.0.0.0:4433",
			want: []listenSpec{{Suite: objects.SuiteNoise3, Bind: "0.0.0.0:4433", AdvertiseHost: "127.0.0.1", Port: 4433}}},
		{name: "explicit IP", spec: "noise3=203.0.113.42:4433",
			want: []listenSpec{{Suite: objects.SuiteNoise3, Bind: "203.0.113.42:4433", AdvertiseHost: "203.0.113.42", Port: 4433}}},
		{name: "reality2 waits for its cover", spec: "reality2-quic=:443", cover: "cover.lab:443", wantErr: "T2.6b"},
		{name: "dns names are not dialed by noise3", spec: "noise3=localhost:4433", wantErr: "IP literal"},
		{name: "port required", spec: "noise3=127.0.0.1", wantErr: "host:port"},
		{name: "port range", spec: "noise3=127.0.0.1:70000", wantErr: "port"},
		{name: "unknown suite", spec: "quic=:443", wantErr: "suite"},
		{name: "no suite", spec: "=:443", wantErr: "suite"},
		{name: "cover without reality2", spec: "noise3=:4433", cover: "cover.lab:443", wantErr: "cover"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseListen(tc.spec, tc.cover)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseListen(%q, %q) = %v, want error containing %q", tc.spec, tc.cover, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseListen(%q): %v", tc.spec, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("listener %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCardFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peer.json")
	in := cardDoc{
		Scope:      "peer",
		Seq:        7,
		NotAfter:   1234,
		NodeID:     strings.Repeat("11", 32),
		DeviceID:   strings.Repeat("22", 32),
		Envelope:   "AAECAwQ=",
		Addresses:  []string{"noise3 127.0.0.1:4433"},
		ServiceIDs: []string{"0xf000/1"},
	}
	if err := writeCardFile(path, in); err != nil {
		t.Fatalf("writeCardFile: %v", err)
	}
	out, err := readCardFile(path)
	if err != nil {
		t.Fatalf("readCardFile: %v", err)
	}
	if out.Scope != in.Scope || out.Seq != in.Seq || out.NotAfter != in.NotAfter ||
		out.NodeID != in.NodeID || out.DeviceID != in.DeviceID || out.Envelope != in.Envelope ||
		len(out.Addresses) != 1 || out.Addresses[0] != in.Addresses[0] {
		t.Fatalf("round trip %+v, want %+v", out, in)
	}
	if _, err := readCardFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("readCardFile accepted a missing file")
	}
}

func TestEchoSpecIsExperimental(t *testing.T) {
	if echoSpec.ID != service.ExperimentalMin {
		t.Fatalf("echo service id %#x, want %#x", echoSpec.ID, service.ExperimentalMin)
	}
	v, ok := echoSpec.Versions[echoVersion]
	if !ok {
		t.Fatalf("echo spec has no version %d", echoVersion)
	}
	var found bool
	for _, ft := range v.Types {
		if ft.Type == echoFrameData {
			found = true
			if ft.Max != echoChunk {
				t.Fatalf("ECHO_DATA max %d, want %d", ft.Max, echoChunk)
			}
		}
	}
	if !found {
		t.Fatalf("echo spec has no frame %#x", echoFrameData)
	}
	if echoDgramType < service.ExperimentalMin || echoDgramType > service.ExperimentalMax {
		t.Fatalf("echo datagram type %#x is outside the experimental range", echoDgramType)
	}
}

// TestInitAndEchoLoopback runs two efv2 profiles on loopback: one listens on noise3, the other
// dials it with its peer card and echoes a small stream and a handful of datagrams. This is the
// same path the test bed's v2-basic scenario drives.
func TestInitAndEchoLoopback(t *testing.T) {
	if testing.Short() {
		t.Skip("the loopback echo test starts two stores")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	serverDir, clientDir := t.TempDir(), t.TempDir()
	if _, err := createIdentity(serverDir, "desktop"); err != nil {
		t.Fatalf("createIdentity(server): %v", err)
	}
	if _, err := createIdentity(clientDir, "desktop"); err != nil {
		t.Fatalf("createIdentity(client): %v", err)
	}

	port := freeUDPPort(t)
	listeners := []listenSpec{{
		Suite:         objects.SuiteNoise3,
		Bind:          fmt.Sprintf("127.0.0.1:%d", port),
		AdvertiseHost: "127.0.0.1",
		Port:          uint32(port),
	}}
	srv, err := openApp(ctx, serverDir, listeners)
	if err != nil {
		t.Fatalf("openApp(server): %v", err)
	}
	defer srv.Close(ctx)
	if err := srv.rt.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	if err := srv.WriteCards(ctx); err != nil {
		t.Fatalf("WriteCards: %v", err)
	}

	cardJSON, err := readCardFile(filepath.Join(serverDir, cardsDirName, "peer.json"))
	if err != nil {
		t.Fatalf("read server card: %v", err)
	}
	cli, err := openApp(ctx, clientDir, nil)
	if err != nil {
		t.Fatalf("openApp(client): %v", err)
	}
	defer cli.Close(ctx)
	if err := cli.rt.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}

	res, err := cli.Echo(ctx, cardJSON, 1<<20, 10)
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if res.BytesSent != 1<<20 || res.BytesReceived != 1<<20 {
		t.Fatalf("echo bytes sent/received %d/%d, want %d", res.BytesSent, res.BytesReceived, 1<<20)
	}
	if res.Suite != uint16(objects.SuiteNoise3) {
		t.Fatalf("suite %d, want noise3 %d", res.Suite, objects.SuiteNoise3)
	}
	if res.Path != "direct" {
		t.Fatalf("path %q, want direct", res.Path)
	}
	if res.DatagramsSent != 10 || res.DatagramsReceived != 10 {
		t.Fatalf("datagrams %d/%d, want 10/10", res.DatagramsSent, res.DatagramsReceived)
	}
	if res.Mbps <= 0 {
		t.Fatalf("throughput %.3f Mbps, want a recorded baseline", res.Mbps)
	}
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("free UDP port: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	if err := pc.Close(); err != nil {
		t.Fatalf("close free UDP port: %v", err)
	}
	return port
}
