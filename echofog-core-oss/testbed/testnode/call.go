package main

// The call side of the test node: it joins a room as the app does, minus the WebRTC bridge to the
// browser engine, speaks and streams made-up units, and counts what arrives from whom. That covers
// what travels between nodes: the room, the trees, relaying, encryption and recovery.

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// callRx counts the units that arrived, by kind and sender, and how late the last one was.
type callRx struct {
	mu       sync.Mutex
	audio    map[string]int
	video    map[string]int
	lastAt   map[string]time.Time
	damaged  int
	talking  chan struct{} // closed to stop the speaker
	streamer chan struct{} // closed to stop the stream
}

func (t *testNode) callRoutes(mux *http.ServeMux) {
	t.rx = &callRx{audio: map[string]int{}, video: map[string]int{}, lastAt: map[string]time.Time{}}
	if t.n.CallManager != nil {
		t.n.CallManager.OnMedia = t.rx.tap
	}
	mux.HandleFunc("/call/join", t.callJoin)
	mux.HandleFunc("/call/leave", t.callLeave)
	mux.HandleFunc("/call/talk", t.callTalk)
	mux.HandleFunc("/call/stream", t.callStream)
	mux.HandleFunc("/call/watch", t.callWatch)
	mux.HandleFunc("/call/rx", t.callRxReport)
	mux.HandleFunc("/call/state", t.callState)
}

// unit is a made-up media unit: a 4-byte sequence number and a body that repeats it, so a
// damaged unit shows.
func unit(seq uint32, size int) []byte {
	b := make([]byte, max(size, 8))
	binary.BigEndian.PutUint32(b, seq)
	for i := 4; i < len(b); i++ {
		b[i] = byte(seq) ^ byte(i)
	}
	return b
}

func unitOK(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	seq := binary.BigEndian.Uint32(b)
	for i := 4; i < len(b); i++ {
		if b[i] != byte(seq)^byte(i) {
			return false
		}
	}
	return true
}

func (r *callRx) tap(p *media.MediaPacket) {
	from := p.Broadcaster.String()[:8]
	r.mu.Lock()
	defer r.mu.Unlock()
	if !unitOK(p.Payload) {
		r.damaged++
		return
	}
	switch p.Kind {
	case media.FrameAudio:
		r.audio[from]++
	case media.FrameVideo:
		r.video[from+"/"+p.StreamID]++
	}
	r.lastAt[from] = time.Now()
}

func (t *testNode) callJoin(w http.ResponseWriter, r *http.Request) {
	var in struct{ CallID, Coordinator string }
	if err := body(r, &in); err != nil {
		reply(w, nil, err)
		return
	}
	if t.n.CallManager == nil {
		reply(w, nil, errors.New("no calls on this node"))
		return
	}
	coord := t.n.ID
	if raw, err := hex.DecodeString(in.Coordinator); err == nil && len(raw) == 32 {
		copy(coord[:], raw)
	}
	roomID, key := call.DeriveRoom(in.CallID)
	prof := tree.PeerNodeProfile{PeerID: t.n.ID, UploadKbps: 20000, MaxRelayChildren: t.n.CallRelayCapacity(),
		RTT: 10 * time.Millisecond, LastHeartbeat: time.Now(), RelayOnly: t.n.CallRelayOnly()}
	reply(w, map[string]bool{"ok": true}, t.n.CallManager.JoinRoom(roomID, coord, key, prof))
}

func (t *testNode) callLeave(w http.ResponseWriter, _ *http.Request) {
	t.rx.mu.Lock()
	for _, c := range []chan struct{}{t.rx.talking, t.rx.streamer} {
		if c != nil {
			close(c)
		}
	}
	t.rx.talking, t.rx.streamer = nil, nil
	t.rx.mu.Unlock()
	reply(w, map[string]bool{"ok": true}, t.n.CallManager.LeaveRoom())
}

// callTalk speaks: 50 audio units a second, as Opus does, until /call/leave or ?stop=1.
func (t *testNode) callTalk(w http.ResponseWriter, r *http.Request) {
	t.rx.mu.Lock()
	if t.rx.talking != nil {
		close(t.rx.talking)
		t.rx.talking = nil
	}
	if r.URL.Query().Get("stop") == "" {
		stop := make(chan struct{})
		t.rx.talking = stop
		go func() {
			tk := time.NewTicker(20 * time.Millisecond)
			defer tk.Stop()
			for seq := uint32(0); ; seq++ {
				select {
				case <-stop:
					return
				case <-tk.C:
					_ = t.n.CallManager.PublishAudio(unit(seq, 120))
				}
			}
		}()
	}
	t.rx.mu.Unlock()
	reply(w, map[string]bool{"ok": true}, nil)
}

// callStream starts a screen share and sends units down its tree: 720p30 as 3000-byte units, 30 a
// second, or with ?q=<quality id> (2160p60, say) the rate of that quality in the 1200-byte units of
// RTP, so a 4K stream puts its real load on the relays.
func (t *testNode) callStream(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	qid := r.URL.Query().Get("q")
	if qid == "" {
		qid = "720p30"
	}
	q, err := tree.QualityByID(qid)
	if err != nil {
		reply(w, nil, err)
		return
	}
	if err := t.n.CallManager.StartStream(id, "screen", q); err != nil {
		reply(w, nil, err)
		return
	}
	size, perSec := 3000, 30.0
	if qid != "720p30" {
		size, perSec = 1200, float64(q.BitrateKbps)*1000/8/1200
	}
	stop := make(chan struct{})
	t.rx.mu.Lock()
	t.rx.streamer = stop
	t.rx.mu.Unlock()
	go func() {
		const tick = 10 * time.Millisecond
		tk := time.NewTicker(tick)
		defer tk.Stop()
		owed := 0.0
		for seq := uint32(0); ; {
			select {
			case <-stop:
				return
			case <-tk.C:
				owed += perSec * tick.Seconds()
				for ; owed >= 1; owed-- {
					_ = t.n.CallManager.PublishVideo(id, unit(seq, size))
					seq++
				}
			}
		}
	}()
	reply(w, map[string]bool{"ok": true}, nil)
}

func (t *testNode) callWatch(w http.ResponseWriter, r *http.Request) {
	reply(w, map[string]bool{"ok": true}, t.n.CallManager.WatchStream(r.URL.Query().Get("id")))
}

// callRxReport is what arrived so far, by sender: units, and seconds since the last one.
func (t *testNode) callRxReport(w http.ResponseWriter, _ *http.Request) {
	t.rx.mu.Lock()
	defer t.rx.mu.Unlock()
	silent := map[string]float64{}
	for k, at := range t.rx.lastAt {
		silent[k] = time.Since(at).Seconds()
	}
	reply(w, map[string]any{"audio": t.rx.audio, "video": t.rx.video, "silentFor": silent, "damaged": t.rx.damaged}, nil)
}

func (t *testNode) callState(w http.ResponseWriter, _ *http.Request) {
	s, ok := t.n.CallManager.Snapshot()
	members := []string{}
	if ok {
		for _, m := range s.Members {
			members = append(members, peerShort(m))
		}
	}
	st := t.n.CallManager.MediaStats()
	reply(w, map[string]any{"inCall": ok && s.Joined, "members": members, "decryptFailures": st.DecryptFailures}, nil)
}

func peerShort(v any) string {
	switch id := v.(type) {
	case types.PeerID:
		return id.String()[:8]
	case string:
		if len(id) > 8 {
			return id[:8]
		}
		return id
	}
	return ""
}
