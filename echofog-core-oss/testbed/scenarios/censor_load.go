package scenarios

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// The traffic the carriers carry in the scenario: what a person does through a tunnel. A session
// loads a few pages (a stream per page: a small request, an answer of varied size, a short think
// time, the same shape as the browser control). The first 16 KB of a flow, which D5-D7 read, are
// therefore the same kind of traffic in the carrier and in the control. A bulk echo after the pages
// is available (bulk > 0), but the scenario does not use it: the uplink of the first 16 KB would
// then be an upload, which is compared with a page load and says nothing about the carrier (a
// measured run with it had D7 flag every TCP flow).

const (
	opPage = 'P'
	opEcho = 'E'

	pagesPerSession = 5
	minSpacing      = 550 * time.Millisecond
	headerLen       = 9 // op, answer size, request length
)

// serveOne answers one stream: a page of n bytes after reading a request, or an echo.
func serveOne(st mux.RawStream) {
	defer st.Close()
	var h [headerLen]byte
	if _, err := io.ReadFull(st, h[:]); err != nil {
		return
	}
	n := int(binary.BigEndian.Uint32(h[1:5]))
	reqLen := int(binary.BigEndian.Uint32(h[5:9]))
	switch h[0] {
	case opEcho:
		_, _ = io.Copy(st, st)
	case opPage:
		if reqLen > headerLen {
			if _, err := io.CopyN(io.Discard, st, int64(reqLen-headerLen)); err != nil {
				return
			}
		}
		page := make([]byte, min(n, maxPageSize))
		for i := range page {
			page[i] = byte('a' + i%26)
		}
		_, _ = st.Write(page)
	}
}

// server accepts carriers and serves their streams.
type server struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func startServer(accept func(context.Context) (mux.Carrier, error)) *server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{cancel: cancel}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := accept(ctx)
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer c.Close()
				go func() { <-ctx.Done(); _ = c.Close() }()
				for {
					st, err := c.AcceptStream(ctx)
					if err != nil {
						return
					}
					go serveOne(st)
				}
			}()
		}
	}()
	return s
}

func (s *server) stop() { s.cancel(); s.wg.Wait() }

func header(op byte, n, reqLen int) []byte {
	h := make([]byte, headerLen)
	h[0] = op
	binary.BigEndian.PutUint32(h[1:], uint32(n))
	binary.BigEndian.PutUint32(h[5:], uint32(reqLen))
	return h
}

// loadPage asks for a page of n bytes and reads all of it.
func loadPage(ctx context.Context, c mux.Carrier, n, reqLen int) error {
	st, err := c.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	defer st.Close()
	req := make([]byte, reqLen)
	_, _ = rand.Read(req)
	copy(req, header(opPage, n, reqLen))
	if _, err := st.Write(req); err != nil {
		return fmt.Errorf("write the request: %w", err)
	}
	page := make([]byte, n)
	got, err := io.ReadFull(st, page)
	if err != nil {
		return fmt.Errorf("page of %d bytes: got %d: %w", n, got, err)
	}
	for i, b := range page {
		if b != byte('a'+i%26) {
			return fmt.Errorf("page of %d bytes: byte %d is wrong", n, i)
		}
	}
	return nil
}

// echoBulk sends n bytes (a multiple of 4096) and checks that the same bytes come back.
func echoBulk(ctx context.Context, c mux.Carrier, n int) error {
	st, err := c.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	defer st.Close()
	pattern := make([]byte, 4096)
	_, _ = rand.Read(pattern)
	if _, err := st.Write(header(opEcho, 0, headerLen)); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 8192)
		got := 0
		for got < n {
			k, err := st.Read(buf)
			for i := 0; i < k; i++ {
				if buf[i] != pattern[(got+i)%4096] {
					done <- errors.New("the echo differs from what was sent")
					return
				}
			}
			got += k
			if err != nil && got < n {
				done <- fmt.Errorf("echo read after %d of %d bytes: %w", got, n, err)
				return
			}
		}
		done <- nil
	}()
	for sent := 0; sent < n; sent += len(pattern) {
		if _, err := st.Write(pattern); err != nil {
			return fmt.Errorf("write after %d of %d bytes: %w", sent, n, err)
		}
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// session is what one client does through one carrier.
func session(ctx context.Context, c mux.Carrier, rng *mrand.Rand, bulk int) error {
	for i := 0; i < pagesPerSession; i++ {
		if err := loadPage(ctx, c, 300+rng.Intn(60000), 400+rng.Intn(300)); err != nil {
			return fmt.Errorf("page %d: %w", i+1, err)
		}
		time.Sleep(time.Duration(5+rng.Intn(35)) * time.Millisecond)
	}
	if bulk > 0 {
		if err := echoBulk(ctx, c, bulk); err != nil {
			return fmt.Errorf("bulk: %w", err)
		}
	}
	return nil
}

// runSessions dials the carrier n times, one after the other, and runs a session on each.
func runSessions(ctx context.Context, n, bulk int, dial func(context.Context) (mux.Carrier, error)) error {
	rng := mrand.New(mrand.NewSource(11))
	var last time.Time
	for i := 0; i < n; i++ {
		// sessions start at least minSpacing apart, so that a fast carrier is not itself a storm of
		// connections (D8 counts more than 20 to one address in 10 s)
		if w := time.Until(last.Add(minSpacing)); w > 0 {
			time.Sleep(w)
		}
		last = time.Now()
		sctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		c, err := dial(sctx)
		if err != nil {
			cancel()
			return fmt.Errorf("session %d: dial: %w", i+1, err)
		}
		stop := context.AfterFunc(sctx, func() { _ = c.Close() })
		err = session(sctx, c, rng, bulk)
		stop()
		_ = c.Close()
		cancel()
		if err != nil {
			return fmt.Errorf("session %d: %w", i+1, err)
		}
	}
	return nil
}
