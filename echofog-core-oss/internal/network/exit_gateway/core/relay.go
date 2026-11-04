package exit_gateway

import (
	"context"
	"errors"
	"io"
	"sync"
)

// relayBufSize is the copy buffer per direction. Bigger than io.Copy's 32 KB: each write to a
// QUIC stream or a socket has a fixed cost, and a proxy moves bulk data most of the time.
const relayBufSize = 64 << 10

var relayBufs = sync.Pool{New: func() any { b := make([]byte, relayBufSize); return &b }}

type closeWriter interface{ CloseWrite() error }

// relay copies both ways between a and b until both directions are done, the way TCP itself
// behaves: when one side finishes sending (EOF), the other side is told so with a half-close
// and the opposite direction keeps flowing, so a client that uploads and then waits for the
// answer still gets it. An error in either direction, or ctx ending, closes both at once.
// countAB and countBA see every chunk as it passes (nil to skip).
func relay(ctx context.Context, a, b io.ReadWriteCloser, countAB, countBA func(int)) error {
	type result struct{ err error }
	done := make(chan result, 2)
	pipe := func(dst io.Writer, src io.Reader, count func(int)) {
		err := copyCounting(dst, src, count)
		if err == nil {
			halfClose(dst)
		}
		done <- result{err}
	}
	go pipe(b, a, countAB)
	go pipe(a, b, countBA)

	closeBoth := func() {
		_ = a.Close()
		_ = b.Close()
	}
	var first error
	for pending := 2; pending > 0; pending-- {
		select {
		case r := <-done:
			if r.err != nil {
				if first == nil {
					first = r.err
				}
				closeBoth() // a broken direction ends the other too
			}
		case <-ctx.Done():
			closeBoth()
			<-done
			<-done
			return ctx.Err()
		}
	}
	closeBoth()
	return first
}

// copyCounting is io.Copy with a pooled buffer that reports each chunk. A clean end of the
// source is nil, like io.Copy.
func copyCounting(dst io.Writer, src io.Reader, count func(int)) error {
	bp := relayBufs.Get().(*[]byte)
	defer relayBufs.Put(bp)
	buf := *bp
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			if w > 0 && count != nil {
				count(w)
			}
			if werr != nil {
				return werr
			}
			if w != n {
				return io.ErrShortWrite
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// halfClose ends the sending side of w when it can do so on its own; a writer without
// half-close is closed fully, which ends the opposite direction too.
func halfClose(w io.Writer) {
	if cw, ok := w.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}
}
