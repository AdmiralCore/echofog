package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// noise3Key derives the masking key pair both ends of a lab run use from the run's seed, so the
// client needs no card file: the real carrier is exercised, only the key distribution is skipped.
func noise3Key(seed int64) (private [32]byte, public []byte) {
	h := sha256.New()
	h.Write([]byte("echofog launchpad noise3 masking key"))
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], uint64(seed))
	h.Write(s[:])
	copy(private[:], h.Sum(nil))
	pub, err := curve25519.X25519(private[:], curve25519.Basepoint)
	if err != nil {
		return private, nil
	}
	return private, pub
}

// noise3Serve runs a real carrier/noise3 listener on pc and echoes every stream until ctx ends.
// It writes ready once the listener is up.
func noise3Serve(ctx context.Context, pc net.PacketConn, seed int64, ready string) error {
	priv, _ := noise3Key(seed)
	profile, err := shape.Builtin(shape.IDDefaultUDP)
	if err != nil {
		_ = pc.Close()
		return err
	}
	l, err := noise3.NewListener(noise3.ListenerConfig{
		Conn:    pc,
		Keys:    []noise3.MaskKey{{Private: priv, NotAfter: time.Now().Add(24 * time.Hour)}},
		Profile: profile,
	})
	if err != nil {
		_ = pc.Close()
		return err
	}
	defer l.Close()
	if ready != "" {
		_ = os.WriteFile(ready, []byte("noise3\n"), 0o644)
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		c, err := l.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			echoStreams(ctx, c)
		}()
	}
}

func echoStreams(ctx context.Context, c mux.Carrier) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		st, err := c.AcceptStream(ctx)
		if err != nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer st.Close()
			_, _ = io.Copy(st, st)
		}()
	}
}

// noise3Client dials the listener at peer with the real carrier/noise3 dialer, opens a stream,
// sends data for d and counts what comes back.
func noise3Client(peer string, seed int64, d time.Duration) (result, error) {
	res := result{Mode: "noise3", Carrier: "noise3"}
	host, portStr, err := net.SplitHostPort(peer)
	if err != nil {
		return res, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return res, err
	}
	_, pub := noise3Key(seed)
	if pub == nil {
		return res, errors.New("masking key")
	}
	card := &objects.Card{Body: &pb.NodeCard{MaskKeys: []*pb.MaskKey{{
		Key:      keys.New(keys.X25519, pub),
		NotAfter: uint64(time.Now().Add(time.Hour).Unix()),
	}}}}
	params, err := objects.Marshal(&pb.SuiteParams{Shape: &pb.ShapeProfile{Id: shape.IDDefaultUDP}})
	if err != nil {
		return res, err
	}
	addr := &pb.Addr{Suite: objects.SuiteNoise3, Host: host, Port: uint32(port), Mask: 1, Params: params}

	ctx, cancel := context.WithTimeout(context.Background(), max(3*time.Second, d)+3*time.Second)
	defer cancel()
	dialCtx, dialCancel := context.WithTimeout(ctx, 3*time.Second)
	defer dialCancel()
	c, err := noise3.NewDialer(noise3.DialerConfig{}).Dial(dialCtx, carrier.DialRequest{Card: card, Addr: addr})
	if err != nil {
		return res, fmt.Errorf("dial: %w", err)
	}
	defer c.Close()
	st, err := c.OpenStream(ctx)
	if err != nil {
		return res, fmt.Errorf("open stream: %w", err)
	}
	defer st.Close()
	res.Connections = 1

	var echoed int64
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := st.Read(buf)
			mu.Lock()
			echoed += int64(n)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	rng := seeded(seed)
	chunk := make([]byte, 1024)
	end := time.Now().Add(d)
	for time.Now().Before(end) || res.Packets == 0 {
		_, _ = rng.Read(chunk)
		if _, err := st.Write(chunk); err != nil {
			return res, fmt.Errorf("write: %w", err)
		}
		res.Bytes += int64(len(chunk))
		res.Packets++
		time.Sleep(5 * time.Millisecond)
	}
	// wait for the echo of everything that was sent
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := echoed
		mu.Unlock()
		if got >= res.Bytes {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	res.EchoedBytes = echoed
	mu.Unlock()
	_ = st.Close()
	return res, nil
}
