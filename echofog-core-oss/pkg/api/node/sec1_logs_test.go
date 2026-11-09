package node

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// syncLog is a log writer safe for the goroutines of two runtimes.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func (w *syncLog) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b.Reset()
}

// TestV2LogsCarryNoScenarioData is the SP-19 guard: a chat, file, call and group scenario runs with
// the runtime log captured, and none of the scenario's own text appears in it.
func TestV2LogsCarryNoScenarioData(t *testing.T) {
	a := newV2Peer(t, "log-a")
	b := newV2Peer(t, "log-b")
	a.other, b.other = b, a
	logs := &syncLog{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a.node.Logger = logger
	b.node.Logger = logger
	a.start()
	b.start()
	// The guard is a guard only when the v2 runtime's own lines reach this writer. It takes the
	// facade logger (NodeApi.Logger) when it is built, so writing through the node's logger alone
	// leaves the buffer empty and the check below passes without seeing a single line.
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "dht: publication")
	}, 10*time.Second, 50*time.Millisecond, "the captured log must carry the v2 runtime's own lines")
	aSeesB, bSeesA := befriend(t, a, b)
	logs.Reset() // startup is not the scenario

	v2StepExchange(t, a, b, aSeesB, bSeesA)

	text := logs.String()
	for _, secret := range []string{
		"привет через v2", // the message
		"привет.bin",      // the file name
		"Команда",         // the group name
		"рабочий чат",     // the group topic
		"в группе",        // a group message
		"call_in_process", // the call
		"coordinator-a",
		"127.0.0.1", // the addresses of the in-process harness
		"203.0.113.10",
		"example.com",
	} {
		require.NotContains(t, text, secret, "the log must not carry scenario data")
	}
}
