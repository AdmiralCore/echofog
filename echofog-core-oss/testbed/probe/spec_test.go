package probe

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSpecReadsHexFlightsAndTheWindow(t *testing.T) {
	spec := fmt.Sprintf(`[{"scenario":"ordinary","network":"tcp","addr":"10.0.0.1:443","cover":"10.0.0.2:443","sni":"cover.lab",
		"first_flight":"%s","valid":"%s","foreign":"%s","window_seconds":1200,"listen_ms":700}]`,
		hex.EncodeToString([]byte{0x16, 3, 1}), hex.EncodeToString([]byte{1}), hex.EncodeToString([]byte{2}))
	cases, err := LoadSpec(strings.NewReader(spec))
	require.NoError(t, err)
	require.Len(t, cases, 1)
	c := cases[0]
	assert.Equal(t, "ordinary", c.Target.Name)
	assert.Equal(t, "tcp", c.Target.Network)
	assert.Equal(t, "10.0.0.1:443", c.Target.Addr)
	assert.Equal(t, "cover.lab", c.Target.SNI)
	assert.Equal(t, []byte{0x16, 3, 1}, c.Inputs.FirstFlight)
	assert.Equal(t, []byte{1}, c.Inputs.Valid)
	assert.Equal(t, []byte{2}, c.Inputs.Foreign)
	assert.Equal(t, 1200*time.Second, c.Inputs.Window)
	assert.Equal(t, 700*time.Millisecond, c.Inputs.Listen)
}

func TestLoadSpecRefusesWhatCannotRun(t *testing.T) {
	for name, spec := range map[string]string{
		"not json":          `{`,
		"bad hex":           `[{"scenario":"ordinary","addr":"a:1","cover":"b:1","first_flight":"zz"}]`,
		"unknown scenario":  `[{"scenario":"bunker","addr":"a:1","cover":"b:1"}]`,
		"no address":        `[{"scenario":"ordinary","cover":"b:1"}]`,
		"ordinary no cover": `[{"scenario":"ordinary","addr":"a:1"}]`,
		"hidden with cover": `[{"scenario":"hidden","network":"udp","addr":"a:1","cover":"b:1"}]`,
		"bad network":       `[{"scenario":"ordinary","network":"sctp","addr":"a:1","cover":"b:1"}]`,
		"empty list":        `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadSpec(strings.NewReader(spec))
			assert.Error(t, err)
		})
	}
}

// A hidden node has no cover: the reference is silence, and any answer is its own.
func TestAHiddenNodeMustNotAnswerAtAll(t *testing.T) {
	quiet, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = quiet.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := quiet.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	tg := Target{Name: ScenarioHidden, Network: "udp", Addr: quiet.LocalAddr().String()}
	rep := Run(context.Background(), tg, Inputs{Listen: 250 * time.Millisecond, Wait: func(time.Duration) {}})
	assert.Equal(t, 0, rep.OwnAnswers(), "%+v", rep.Outcomes)
	for _, o := range rep.Outcomes {
		if o.Skipped == "" {
			assert.Equal(t, "silence", o.Cover.Class, "%s/%s: the reference of a hidden node is silence", o.Detector, o.Case)
		}
	}

	talker, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = talker.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			_, from, err := talker.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = talker.WriteTo([]byte{0x40, 1, 2, 3}, from)
		}
	}()
	tg.Addr = talker.LocalAddr().String()
	rep = Run(context.Background(), tg, Inputs{Listen: 250 * time.Millisecond, Wait: func(time.Duration) {}})
	assert.Greater(t, rep.OwnAnswers(), 0, "a hidden node that answers a stranger has given itself away")
}

func TestRunSpecWritesReportsAndCountsLeaks(t *testing.T) {
	cv := newCover(t)
	good := newNode(t, cv.coverAddr, nodeBehaviour{})
	first := recordClientHello(t, cv.coverAddr, "cover.lab")
	spec := fmt.Sprintf(`[{"scenario":"ordinary","network":"tcp","addr":%q,"cover":%q,"sni":"cover.lab","first_flight":%q,"window_seconds":1,"listen_ms":400}]`,
		good.addr, cv.coverAddr, hex.EncodeToString(first))
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	own, err := RunSpec(ctx, strings.NewReader(spec), &out)
	require.NoError(t, err)
	assert.Equal(t, 0, own)
	var reps []Report
	require.NoError(t, json.Unmarshal(out.Bytes(), &reps))
	require.Len(t, reps, 1)
	assert.Equal(t, "ordinary", reps[0].Target)
	assert.NotEmpty(t, reps[0].Outcomes)

	leaky := newNode(t, cv.coverAddr, nodeBehaviour{own: func(first []byte) []byte {
		if len(first) < 5 || first[0] != 0x16 {
			return []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x46}
		}
		return nil
	}})
	spec = fmt.Sprintf(`[{"scenario":"ordinary","network":"tcp","addr":%q,"cover":%q,"sni":"cover.lab","first_flight":%q,"window_seconds":1,"listen_ms":400}]`,
		leaky.addr, cv.coverAddr, hex.EncodeToString(first))
	out.Reset()
	own, err = RunSpec(ctx, strings.NewReader(spec), &out)
	require.NoError(t, err)
	assert.Greater(t, own, 0)
}
