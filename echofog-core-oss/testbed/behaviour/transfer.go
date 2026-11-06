package behaviour

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
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

// laneKey derives the masking key pair of a lab run from its seed, so the client needs no card
// file: the real carrier is exercised, only the key distribution is skipped.
func laneKey(seed int64) (private [32]byte, public []byte, err error) {
	h := sha256.New()
	h.Write([]byte("echofog launchpad behaviour masking key"))
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], uint64(seed))
	h.Write(s[:])
	copy(private[:], h.Sum(nil))
	public, err = curve25519.X25519(private[:], curve25519.Basepoint)
	return private, public, err
}

// StartEcho runs a noise3 echo server on loopback and returns its address and a stop function.
// The key comes from seed; a client must dial with the same seed.
func StartEcho(seed int64) (addr string, stop func(), err error) {
	priv, _, err := laneKey(seed)
	if err != nil {
		return "", nil, err
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	profile, err := shape.Builtin(shape.IDDefaultUDP)
	if err != nil {
		_ = pc.Close()
		return "", nil, err
	}
	l, err := noise3.NewListener(noise3.ListenerConfig{
		Conn: pc, Keys: []noise3.MaskKey{{Private: priv, NotAfter: time.Now().Add(24 * time.Hour)}}, Profile: profile,
	})
	if err != nil {
		_ = pc.Close()
		return "", nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept(ctx)
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				// a client that vanished leaves its streams reading for ever; stopping the server
				// closes the carrier under them
				go func() {
					<-ctx.Done()
					_ = c.Close()
				}()
				echoAll(ctx, c)
			}()
		}
	}()
	return pc.LocalAddr().String(), func() {
		cancel()
		_ = l.Close()
		wg.Wait()
	}, nil
}

func echoAll(ctx context.Context, c mux.Carrier) {
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

// TransferResult is one transfer through the carrier.
type TransferResult struct {
	Connected   bool
	ConnectTime time.Duration
	Total       time.Duration
	Bytes       int64
	Echoed      int64
	MaxGap      time.Duration // the longest time without a byte coming back
	Err         string
}

// RunTransfer dials addr with the real noise3 dialer, sends n bytes on a stream and counts what
// the echo returns. hook is called, from the writer, with the bytes sent so far.
func RunTransfer(ctx context.Context, addr string, seed int64, n int64, hook func(sent int64)) TransferResult {
	var res TransferResult
	_, pub, err := laneKey(seed)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	port, _ := strconv.Atoi(portStr)
	card := &objects.Card{Body: &pb.NodeCard{MaskKeys: []*pb.MaskKey{{
		Key: keys.New(keys.X25519, pub), NotAfter: uint64(time.Now().Add(time.Hour).Unix()),
	}}}}
	params, err := objects.Marshal(&pb.SuiteParams{Shape: &pb.ShapeProfile{Id: shape.IDDefaultUDP}})
	if err != nil {
		res.Err = err.Error()
		return res
	}
	a := &pb.Addr{Suite: objects.SuiteNoise3, Host: host, Port: uint32(port), Mask: 1, Params: params}

	start := time.Now()
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := noise3.NewDialer(noise3.DialerConfig{}).Dial(dialCtx, carrier.DialRequest{Card: card, Addr: a})
	if err != nil {
		res.Err = "dial: " + err.Error()
		res.Total = time.Since(start)
		return res
	}
	defer c.Close()
	// a write blocked on a dead path has no deadline of its own: closing the carrier ends it
	stopClose := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stopClose()
	res.Connected, res.ConnectTime = true, time.Since(start)
	st, err := c.OpenStream(ctx)
	if err != nil {
		res.Err = "open stream: " + err.Error()
		return res
	}
	defer st.Close()

	var mu sync.Mutex
	last := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 16<<10)
		var echoed int64
		for echoed < n {
			k, err := st.Read(buf)
			mu.Lock()
			if k > 0 {
				if gap := time.Since(last); gap > res.MaxGap {
					res.MaxGap = gap
				}
				last = time.Now()
				echoed += int64(k)
				res.Echoed = echoed
			}
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	chunk := make([]byte, 4096)
	rand.New(rand.NewSource(seed)).Read(chunk)
	for sent := int64(0); sent < n; {
		k := int64(len(chunk))
		if n-sent < k {
			k = n - sent
		}
		if _, err := st.Write(chunk[:k]); err != nil {
			res.Err = "write: " + err.Error()
			break
		}
		sent += k
		res.Bytes = sent
		if hook != nil {
			hook(sent)
		}
	}
	select {
	case <-done:
	case <-ctx.Done():
		res.Err = errors.Join(errors.New("transfer"), ctx.Err()).Error()
	}
	mu.Lock()
	defer mu.Unlock()
	res.Total = time.Since(start)
	return res
}

func (r TransferResult) String() string {
	return fmt.Sprintf("connected=%v bytes=%d echoed=%d total=%s maxgap=%s err=%q", r.Connected, r.Bytes, r.Echoed, r.Total.Round(time.Millisecond), r.MaxGap.Round(time.Millisecond), r.Err)
}

// once turns f into a hook that fires the first time 128 KiB have been sent, and never again: a
// path change is an event, not a state to hold.
func once(f func(*Netem)) func(*Netem, int64) {
	var fired bool
	return func(n *Netem, sent int64) {
		if !fired && sent >= 128<<10 {
			fired = true
			f(n)
		}
	}
}
