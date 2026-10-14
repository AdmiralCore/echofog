package network

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/testutil"
)

func TestHandshake_Negotiator(t *testing.T) {
	imOne, storeOne := testutil.SetupTestIdentity(t)
	defer storeOne.CloseAndClear()
	imTwo, storeTwo := testutil.SetupTestIdentity(t)
	defer storeTwo.CloseAndClear()

	_, tlsOne := testutil.GetRandomKeyAndTls(t)
	_, tlsTwo := testutil.GetRandomKeyAndTls(t)

	myTr, err := NewQUICTransport("127.0.0.1:0", tlsOne, GetQuicConfig(), imOne, config.CurrentProtocolVersion)
	if err != nil {
		t.Fatalf("Failed to create myTr: %v", err)
	}
	defer myTr.Close()
	friendTr, err := NewQUICTransport("127.0.0.1:0", tlsTwo, GetQuicConfig(), imTwo, config.CurrentProtocolVersion)
	if err != nil {
		t.Fatalf("Failed to create friendTr: %v", err)
	}
	defer friendTr.Close()

	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = myTr.ListenEarly(parentCtx)
	if err != nil {
		t.Fatal(err)
	}

	dialCtx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()

	err = friendTr.DialEarly(dialCtx, myTr.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
}

func TestDirectSocket_HighThroughput(t *testing.T) {
	imOne, storeOne := testutil.SetupTestIdentity(t)
	defer storeOne.CloseAndClear()
	imTwo, storeTwo := testutil.SetupTestIdentity(t)
	defer storeTwo.CloseAndClear()

	_, tlsOne := testutil.GetRandomKeyAndTls(t)
	_, tlsTwo := testutil.GetRandomKeyAndTls(t)

	directOpts := TransportOptions{
		DirectSocket:      true,
		SocketBufferBytes: 16 * 1024 * 1024,
	}

	addrOne := "127.0.0.1:0"
	addrTwo := "127.0.0.1:0"

	myTr, err := NewQUICTransportWithOptions(addrOne, tlsOne, GetQuicConfig(), imOne, config.CurrentProtocolVersion, directOpts)
	if err != nil {
		t.Fatalf("Failed to create myTr in DirectSocket mode: %v", err)
	}
	defer myTr.Close()

	if myTr.Mux != nil {
		t.Fatalf("expected Mux to be nil in DirectSocket mode, got non-nil")
	}
	if !myTr.directSocket {
		t.Fatalf("expected directSocket to be true")
	}

	stunConn := myTr.GetSTUNConn()
	if stunConn == nil {
		t.Fatalf("expected non-nil ephemeral STUN conn in DirectSocket mode")
	}

	friendTr, err := NewQUICTransportWithOptions(addrTwo, tlsTwo, GetQuicConfig(), imTwo, config.CurrentProtocolVersion, directOpts)
	if err != nil {
		t.Fatalf("Failed to create friendTr in DirectSocket mode: %v", err)
	}
	defer friendTr.Close()

	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = myTr.ListenEarly(parentCtx)
	if err != nil {
		t.Fatalf("ListenEarly failed in DirectSocket mode: %v", err)
	}

	dialCtx, dialCancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer dialCancel()

	err = friendTr.DialEarly(dialCtx, myTr.Addr().String())
	if err != nil {
		t.Fatalf("DialEarly failed in DirectSocket mode: %v", err)
	}

	select {
	case ev := <-myTr.ConnChan():
		if ev.Conn == nil {
			t.Fatalf("expected valid quic.Conn on incoming connection")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for incoming connection in DirectSocket mode")
	}
}
