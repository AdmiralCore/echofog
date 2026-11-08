package scenarios

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTheEchoServiceReturnsEveryByteAndMeasuresThePauses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	w := NewWorld(t.TempDir())
	defer w.Close()
	srv, err := w.Add(NodeSpec{Name: "srv", Server: true, Listen: true})
	require.NoError(t, err)
	cli, err := w.Add(NodeSpec{Name: "cli"})
	require.NoError(t, err)
	RegisterEcho(srv)
	RegisterEcho(cli)
	require.NoError(t, w.StartAll(ctx))
	_, _, err = cli.Dial(ctx, srv)
	require.NoError(t, err)

	var seen int64
	run := cli.EchoStream(ctx, srv, EchoOptions{Total: 1 << 20, Hook: func(sent int64) { seen = sent }})
	require.NoError(t, run.Err)
	assert.EqualValues(t, 1<<20, run.Sent)
	assert.EqualValues(t, 1<<20, run.Received)
	assert.EqualValues(t, 1<<20, seen, "the hook saw the last byte sent")
	assert.Positive(t, run.MaxPause)
	assert.Less(t, run.MaxPause, 10*time.Second)
}

func TestTheEchoReportsATransferThatCannotFinish(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	w := NewWorld(t.TempDir())
	defer w.Close()
	srv, err := w.Add(NodeSpec{Name: "srv", Server: true, Listen: true})
	require.NoError(t, err)
	cli, err := w.Add(NodeSpec{Name: "cli"})
	require.NoError(t, err)
	RegisterEcho(srv)
	RegisterEcho(cli)
	require.NoError(t, w.StartAll(ctx))
	_, _, err = cli.Dial(ctx, srv)
	require.NoError(t, err)
	short, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	run := cli.EchoStream(short, srv, EchoOptions{Total: 1 << 30, Pace: 50 * time.Millisecond})
	assert.Error(t, run.Err)
	assert.Less(t, run.Received, run.Sent+1)
}
