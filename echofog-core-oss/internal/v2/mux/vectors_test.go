package mux

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// TestMuxSequenceVectors feeds the whole-connection byte sequences of frames.json
// (mux_sequences, wire.md §5.2) to a mux connection and checks the streams it ends with.
func TestMuxSequenceVectors(t *testing.T) {
	b, err := os.ReadFile("../testdata/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Seqs []struct {
			Name    string `json:"name"`
			Sender  string `json:"sender"`
			Bytes   string `json:"bytes"`
			Streams []struct {
				ID      uint64  `json:"id"`
				DataHex string  `json:"data_hex"`
				Fin     bool    `json:"fin"`
				Reset   *uint64 `json:"reset"`
			} `json:"streams"`
		} `json:"mux_sequences"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Seqs) < 2 {
		t.Fatalf("mux_sequences: %d entries", len(v.Seqs))
	}
	for _, sq := range v.Seqs {
		t.Run(sq.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(sq.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			a, bb := net.Pipe()
			go func() { _, _ = io.Copy(io.Discard, a) }() // the receiver's window updates
			go func() { _, _ = a.Write(raw) }()
			// The receiver is the opposite role of the sender.
			rc := NewConn(bb, ConnConfig{Client: sq.Sender == "server"})
			t.Cleanup(func() { rc.Close(); a.Close() })
			ctx := ctxT(t)
			got := map[uint64]RawStream{}
			wantN := 0
			for _, w := range sq.Streams {
				if w.Reset == nil {
					wantN++
				}
			}
			for len(got) < wantN {
				st, err := rc.AcceptStream(ctx)
				if err != nil {
					t.Fatal(err)
				}
				got[st.ID()] = st
			}
			// A stream that is reset before the receiver accepts it may be dropped: wire.md §5.2
			// keeps only the data that arrived before the reset readable. Accept it if it is there.
			for _, w := range sq.Streams {
				if w.Reset == nil || got[w.ID] != nil {
					continue
				}
				actx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
				if st, err := rc.AcceptStream(actx); err == nil {
					got[st.ID()] = st
				}
				cancel()
			}
			// Streams are accepted in increasing ID order of the opener (wire.md §5.2).
			for _, want := range sq.Streams {
				st := got[want.ID]
				if st == nil {
					if want.Reset != nil {
						continue
					}
					t.Fatalf("stream %d was not accepted", want.ID)
				}
				_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
				data, err := io.ReadAll(st)
				if want.Reset != nil {
					var re *ResetError
					if !errors.As(err, &re) || re.Code != *want.Reset {
						t.Fatalf("stream %d: err %v, want reset %d", want.ID, err, *want.Reset)
					}
					continue
				}
				if err != nil {
					t.Fatalf("stream %d: %v", want.ID, err)
				}
				if hex.EncodeToString(data) != want.DataHex {
					t.Fatalf("stream %d: data %x, want %s", want.ID, data, want.DataHex)
				}
			}
		})
	}
}
