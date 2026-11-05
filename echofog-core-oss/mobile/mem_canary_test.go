package mobile

// Memory canary of T12.6 (not part of the normal run). It needs two processes so the client's
// peak RSS is not mixed with the hosts':
//
//	ECHOFOG_MEM_CANARY=host   ECHOFOG_MEM_DIR=/tmp/m go test -run TestMemCanary ./mobile/   # hosts
//	ECHOFOG_MEM_CANARY=client ECHOFOG_MEM_DIR=/tmp/m go test -run TestMemCanary ./mobile/   # android profile client
//
// The host writes card0..card9 (reality2-quic) and noise3.json (noise3), the client dials the
// ten, streams ECHOFOG_MEM_MB megabytes (default 1024) through the noise3 host and prints the
// peak RSS (VmHWM) after each phase. Linux only.

import (
	"context"
	"crypto/ecdh"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

const (
	memEchoID    = service.ExperimentalMin
	memEchoFrame = 0x40
	memChunk     = 64 << 10
)

var memEchoSpec = service.Spec{
	ID: memEchoID, Name: "echo",
	Versions: map[uint64]service.Version{1: {Types: []service.FrameType{{Type: memEchoFrame, Name: "ECHO_DATA", Max: memChunk}}}},
}

func registerMemEcho(rt *node.Runtime) {
	rt.Register(memEchoSpec, func(_ context.Context, _ node.Peer, st *mux.Stream) {
		defer st.Close()
		for {
			f, err := st.ReadFrame()
			if err != nil {
				return
			}
			if f.Type != memEchoFrame {
				_ = st.CloseWithCode(frame.CodeProtocolViolation, "unexpected frame")
				return
			}
			if err := st.WriteFrame(memEchoFrame, 0, f.Payload); err != nil {
				return
			}
		}
	})
}

func vmHWM() string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "n/a"
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmHWM:") || strings.HasPrefix(l, "VmRSS:") {
			if strings.HasPrefix(l, "VmHWM:") {
				return strings.TrimSpace(strings.TrimPrefix(l, "VmHWM:"))
			}
		}
	}
	return "n/a"
}

func TestMemCanary(t *testing.T) {
	role, dir := os.Getenv("ECHOFOG_MEM_CANARY"), os.Getenv("ECHOFOG_MEM_DIR")
	if role == "" || dir == "" {
		t.Skip("set ECHOFOG_MEM_CANARY=host|client and ECHOFOG_MEM_DIR")
	}
	switch role {
	case "host":
		memHost(t, dir)
	case "client":
		memClient(t, dir)
	default:
		t.Fatalf("role %q", role)
	}
}

func memHost(t *testing.T, dir string) {
	cover := newCover(t)
	cert := testCert(t)
	ctx := context.Background()
	var insts []*instance
	mk := func(name string, noise bool) {
		inst, err := open(config{DataDir: t.TempDir(), Platform: "server"}, nil, func(dev *identity.Device) ([]cards.Listener, []node.ListenerSource, error) {
			var priv *ecdh.PrivateKey
			if err := dev.Masking().WithPrivate(func(b []byte) (err error) { priv, err = ecdh.X25519().NewPrivateKey(b); return }); err != nil {
				return nil, nil, err
			}
			mask := keys.New(keys.X25519, priv.PublicKey().Bytes())
			if noise {
				var raw [32]byte
				copy(raw[:], priv.Bytes())
				port := 20000 + len(insts)
				return []cards.Listener{{Suite: objects.SuiteNoise3, Host: "127.0.0.1", Port: uint32(port), Mask: mask}},
					[]node.ListenerSource{node.Noise3Listener{Addr: "127.0.0.1:" + strconv.Itoa(port), Masks: []noise3.MaskKey{{Private: raw}}}}, nil
			}
			udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				return nil, nil, err
			}
			port := udp.LocalAddr().(*net.UDPAddr).Port
			ln, err := reality2.Listen(udp, reality2.ListenerConfig{
				Cover: reality2.CoverProfile{Group: tls.X25519, Cert: &cert}, Masks: []reality2.MaskKey{{Private: priv}},
				Addrs: []string{reality2.AddressString("127.0.0.1", uint32(port))}, CoverAddr: cover,
			})
			if err != nil {
				return nil, nil, err
			}
			return []cards.Listener{{Suite: objects.SuiteReality2QUIC, Host: "127.0.0.1", Port: uint32(port), SNI: fmt.Sprintf("cover%d.example", len(insts)), Mask: mask}},
				[]node.ListenerSource{reality2Source{ln}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		registerMemEcho(inst.rt)
		if err := inst.rt.Start(ctx); err != nil {
			t.Fatal(err)
		}
		ic, err := inst.cards.Current(ctx, objects.ScopePeer)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"envelope":"`+base64.StdEncoding.EncodeToString(ic.Envelope)+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		insts = append(insts, inst)
	}
	for i := 0; i < 10; i++ {
		mk(fmt.Sprintf("card%d", i), false)
	}
	mk("noise3.json", true)
	defer func() {
		for _, i := range insts {
			_ = i.stop()
		}
	}()
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func memClient(t *testing.T, dir string) {
	for i := 0; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if i > 300 {
			t.Fatal("hosts not ready")
		}
		time.Sleep(200 * time.Millisecond)
	}
	mb := 1024
	if v, err := strconv.Atoi(os.Getenv("ECHOFOG_MEM_MB")); err == nil {
		mb = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	countryOf = func(netip.Addr) (string, bool) { return "lab", true } // same country: noise3 allowed
	cb := &fakeCallbacks{}
	inst, err := open(config{DataDir: t.TempDir(), Platform: "android", LowPower: true}, cb, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerMemEcho(inst.rt)
	if err := inst.rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer inst.stop()
	fmt.Printf("MEM start            VmHWM=%s\n", vmHWM())
	for i := 0; i < 10; i++ {
		b, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("card%d", i)))
		if _, err := inst.dial(string(b)); err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
	}
	fmt.Printf("MEM 10 sessions      VmHWM=%s\n", vmHWM())

	b, _ := os.ReadFile(filepath.Join(dir, "noise3.json"))
	var doc struct{ Envelope string }
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(doc.Envelope)
	card, err := objects.VerifyCard(raw, inst.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	dev := node.DeviceID(card.DeviceID)
	if _, _, err := inst.rt.Session(ctx, dev, node.DialOpt{Card: card}); err != nil {
		t.Fatal(err)
	}
	_, st, err := inst.rt.Open(ctx, dev, memEchoID, node.DialOpt{Card: card})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	total := int64(mb) << 20
	payload := make([]byte, memChunk)
	errc := make(chan error, 1)
	go func() {
		var got int64
		for got < total {
			f, err := st.ReadFrame()
			if err != nil {
				errc <- err
				return
			}
			got += int64(len(f.Payload))
		}
		errc <- nil
	}()
	start := time.Now()
	for sent := int64(0); sent < total; sent += memChunk {
		if err := st.WriteFrame(memEchoFrame, 0, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	fmt.Printf("MEM %d MB via noise3 VmHWM=%s (%.1fs)\n", mb, vmHWM(), time.Since(start).Seconds())
}
