// Package carriertest is the shared conformance suite for mux.Carrier implementations
// (E2 carriers, wire.md §1 and §10). A carrier package calls Run from its tests:
//
//	func TestCarrier(t *testing.T) {
//		carriertest.Run(t, func(t *testing.T) (client, server mux.Carrier) {
//			p := memcarrier.NewQUIC()
//			return p.Client, p.Server
//		})
//	}
//
// The pair must be independent per call. Run checks both directions of streams, FIN and Reset,
// datagrams, channel binding, sealed mode and connection teardown. It uses real deadlines only to
// fail a broken implementation instead of hanging the test binary.
package carriertest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

const stepTimeout = 5 * time.Second

// NewPair creates one connected client/server carrier pair.
type NewPair func(t *testing.T) (client, server mux.Carrier)

// Run executes the conformance suite on a fresh pair for every subtest.
func Run(t *testing.T, newPair NewPair) {
	t.Helper()
	if newPair == nil {
		t.Fatal("carriertest: nil NewPair")
	}

	t.Run("roles", func(t *testing.T) {
		client, server := pair(t, newPair)
		if !client.IsClient() {
			t.Error("client.IsClient() = false")
		}
		if server.IsClient() {
			t.Error("server.IsClient() = true")
		}
	})

	t.Run("client_to_server", func(t *testing.T) {
		client, server := pair(t, newPair)
		exchange(t, client, server, []byte("hello from the client"))
	})

	t.Run("server_to_client", func(t *testing.T) {
		client, server := pair(t, newPair)
		exchange(t, server, client, []byte("hello from the server"))
	})

	t.Run("half_close", func(t *testing.T) {
		client, server := pair(t, newPair)
		ctx, cancel := stepContext(t)
		defer cancel()

		cs, err := client.OpenStream(ctx)
		if err != nil {
			t.Fatalf("client.OpenStream: %v", err)
		}
		if _, err := cs.Write([]byte("ping")); err != nil {
			t.Fatalf("client write: %v", err)
		}
		if err := cs.CloseWrite(); err != nil {
			t.Fatalf("client CloseWrite: %v", err)
		}
		ss, err := server.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("server.AcceptStream: %v", err)
		}
		if got := readAll(t, ss); !bytes.Equal(got, []byte("ping")) {
			t.Fatalf("server read = %q, want ping", got)
		}
		// The server half stays open after the client's FIN.
		if _, err := ss.Write([]byte("pong")); err != nil {
			t.Fatalf("server write after peer FIN: %v", err)
		}
		if err := ss.CloseWrite(); err != nil {
			t.Fatalf("server CloseWrite: %v", err)
		}
		if got := readAll(t, cs); !bytes.Equal(got, []byte("pong")) {
			t.Fatalf("client read = %q, want pong", got)
		}
	})

	t.Run("reset", func(t *testing.T) {
		client, server := pair(t, newPair)
		ctx, cancel := stepContext(t)
		defer cancel()

		cs, err := client.OpenStream(ctx)
		if err != nil {
			t.Fatalf("client.OpenStream: %v", err)
		}
		// A QUIC stream is created on the peer by its first STREAM frame, so a
		// stream has to carry at least one byte before it can be accepted. The
		// byte is irrelevant to the reset being tested.
		if _, err := cs.Write([]byte("x")); err != nil {
			t.Fatalf("client write: %v", err)
		}
		ss, err := server.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("server.AcceptStream: %v", err)
		}
		cs.Reset(0x2a)
		if err := ss.SetReadDeadline(time.Now().Add(stepTimeout)); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		_, err = io.ReadAll(ss)
		if err == nil {
			t.Fatal("read after Reset returned no error")
		}
		var re *mux.ResetError
		if !errors.As(err, &re) {
			t.Fatalf("read after Reset error = %v, want *mux.ResetError", err)
		}
		if re.Code != 0x2a {
			t.Fatalf("reset code = %d, want 0x2a", re.Code)
		}
		if !re.Remote {
			t.Fatal("reset reported as local on the receiving side")
		}
	})

	t.Run("datagrams", func(t *testing.T) {
		client, server := pair(t, newPair)
		ctx, cancel := stepContext(t)
		defer cancel()

		send := []byte("datagram from the client")
		if err := client.SendDatagram(send); err != nil {
			t.Fatalf("client.SendDatagram: %v", err)
		}
		got, err := server.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatalf("server.ReceiveDatagram: %v", err)
		}
		if !bytes.Equal(got, send) {
			t.Fatalf("server datagram = %q, want %q", got, send)
		}
		back := []byte("datagram from the server")
		if err := server.SendDatagram(back); err != nil {
			t.Fatalf("server.SendDatagram: %v", err)
		}
		got, err = client.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatalf("client.ReceiveDatagram: %v", err)
		}
		if !bytes.Equal(got, back) {
			t.Fatalf("client datagram = %q, want %q", got, back)
		}
	})

	t.Run("channel_binding", func(t *testing.T) {
		client, server := pair(t, newPair)
		first := client.ChannelBinding()
		if first != server.ChannelBinding() {
			t.Fatal("channel binding differs between the two sides")
		}
		otherClient, _ := pair(t, newPair)
		if otherClient.ChannelBinding() == first {
			t.Fatal("channel binding is the same for two different connections")
		}
	})

	t.Run("sealed", func(t *testing.T) {
		client, server := pair(t, newPair)
		if client.Sealed() != server.Sealed() {
			t.Fatal("Sealed() differs between the two sides")
		}
	})

	t.Run("done_after_close", func(t *testing.T) {
		client, server := pair(t, newPair)
		if err := client.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		waitDone(t, client, "client")
		waitDone(t, server, "server")
	})
}

func pair(t *testing.T, newPair NewPair) (mux.Carrier, mux.Carrier) {
	t.Helper()
	client, server := newPair(t)
	if client == nil || server == nil {
		t.Fatal("NewPair returned a nil carrier")
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

func stepContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), stepTimeout)
}

func exchange(t *testing.T, from, to mux.Carrier, payload []byte) {
	t.Helper()
	ctx, cancel := stepContext(t)
	defer cancel()

	s, err := from.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := s.Write(payload); err != nil {
		t.Fatalf("stream write: %v", err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	rs, err := to.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	if got := readAll(t, rs); !bytes.Equal(got, payload) {
		t.Fatalf("stream read = %q, want %q", got, payload)
	}
	if err := rs.Close(); err != nil {
		t.Fatalf("stream Close: %v", err)
	}
}

func readAll(t *testing.T, s mux.RawStream) []byte {
	t.Helper()
	if err := s.SetReadDeadline(time.Now().Add(stepTimeout)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	b, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("stream read: %v", err)
	}
	return b
}

func waitDone(t *testing.T, c mux.Carrier, side string) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(stepTimeout):
		t.Fatalf("%s.Done() did not close after Close", side)
	}
}
