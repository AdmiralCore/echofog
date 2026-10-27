package chatlogtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/stretchr/testify/require"
)

// TestUIBridge is not a test: it starts two real nodes, Alice and Bob, who are contacts and share
// a group, and serves each one's chat API and events over HTTP, so that the desktop UI can be
// driven against real nodes in a browser:
//
//	ECHOFOG_UI_BRIDGE=1 go test ./pkg/chatlogtest -run TestUIBridge -timeout 0 -v
//	(frontend) npm run dev, then open /?mock&bridge=7601 (Alice) and /?mock&bridge=7602 (Bob)
//
// The page keeps the mock for everything but chats (see bridge in frontend/src/lib/dev/mock.js).
// POST /quit, or ECHOFOG_UI_BRIDGE_MINUTES (default 60), ends it.
func TestUIBridge(t *testing.T) {
	if os.Getenv("ECHOFOG_UI_BRIDGE") == "" {
		t.Skip("a tool for driving the UI by hand: set ECHOFOG_UI_BRIDGE=1")
	}
	aliceN, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob")
	for _, a := range []*node.NodeApi{alice, bob} {
		_, err := a.Conversations() // the desktop opens the logs when it starts the node
		require.NoError(t, err)
	}
	require.NoError(t, bob.ConnectToPeer(joinOf(aliceN)))
	eventually(t, "connect", func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 })
	require.NoError(t, alice.NewContact(dto.ContactDTO{PeerID: bobN.ID.String(), PubKey: bob.GetMyPubKey(), Alias: "Bob"}))
	require.NoError(t, bob.NewContact(dto.ContactDTO{PeerID: aliceN.ID.String(), PubKey: alice.GetMyPubKey(), Alias: "Alice"}))

	// a first message each way tells both that the other speaks the log
	_, err := alice.ChatSend(dmRef(bobN.ID.String()), node.SendRequest{Text: "Hi Bob, testing the new chat screen"})
	require.NoError(t, err)
	_, err = bob.ChatSend(dmRef(aliceN.ID.String()), node.SendRequest{Text: "Hi Alice, here"})
	require.NoError(t, err)

	g, err := alice.CreateGroup("Crew", "", "group")
	require.NoError(t, err)
	invite, err := alice.GetGroupInvite(g.ID)
	require.NoError(t, err)
	_, err = bob.JoinGroup(invite)
	require.NoError(t, err)
	eventually(t, "both in the group", func() bool {
		m, err := bob.ListGroupMembers(g.ID)
		return err == nil && len(m) == 2
	})
	for _, a := range []*node.NodeApi{alice, bob} {
		_, err := a.SendGroupMessage(g.ID, "hello group")
		require.NoError(t, err)
	}
	eventually(t, "the group runs on the log", func() bool { return upgraded(t, alice, g.ID) && upgraded(t, bob, g.ID) })

	quit := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(quit) }) }
	for _, b := range []struct {
		name string
		api  *node.NodeApi
		port string
	}{{"alice", alice, "7601"}, {"bob", bob, "7602"}} {
		ln, err := net.Listen("tcp", "127.0.0.1:"+b.port)
		require.NoError(t, err)
		srv := &http.Server{Handler: newBridge(b.api, stop)}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
		t.Logf("%s: http://127.0.0.1:%s (peer %s)", b.name, b.port, b.api.GetMyID()[:12])
	}
	minutes := 60
	if v := os.Getenv("ECHOFOG_UI_BRIDGE_MINUTES"); v != "" {
		_, _ = fmt.Sscan(v, &minutes)
	}
	t.Log("bridge up")
	select {
	case <-quit:
	case <-time.After(time.Duration(minutes) * time.Minute):
	}
}

// bridged are the desktop methods the bridge serves, by the NodeApi method behind each.
var bridged = map[string]string{
	"GetMyID": "GetMyID", "GetMyPubKey": "GetMyPubKey", "GetContacts": "GetContacts",
	"GetBlockedPeers": "GetBlockedPeers", "GetMessageRequests": "GetMessageRequests",
	"AcceptMessageRequest": "AcceptMessageRequest", "DismissMessageRequest": "DismissMessageRequest",
	"BlockPeer": "BlockPeer", "UnblockPeer": "UnblockPeer",
	"ListGroups": "ListGroups", "ListGroupMembers": "ListGroupMembers", "GetGroupInvite": "GetGroupInvite",
	"SetGroupRole": "SetGroupRole",
	"ChatList":     "Conversations", "ChatHistory": "ChatHistory", "ChatThread": "ChatThread", "ChatPins": "ChatPins",
	"ChatMessage": "ChatMessage", "ChatSend": "ChatSend", "ChatEdit": "ChatEdit", "ChatDelete": "ChatDelete",
	"ChatReact": "ChatReact", "ChatPin": "ChatPin", "ChatMarkRead": "ChatMarkRead",
}

type bridge struct {
	api  reflect.Value
	stop func()
	mu   sync.Mutex
	subs map[chan []byte]bool
}

func newBridge(api *node.NodeApi, stop func()) http.Handler {
	b := &bridge{api: reflect.ValueOf(api), stop: stop, subs: map[chan []byte]bool{}}
	api.SubscribeConversations(func(ch node.ConvChange) { b.emit("conv:change", ch) })
	api.OnMessageState(func(ev node.MessageStateDTO) { b.emit("message:state", ev) })
	api.SubscribeGroupEvents(func(ev node.GroupEvent) { b.emit("group:event", ev) })
	api.OnPeerState(func(ev node.PeerStateDTO) { b.emit("peer:state", ev) })
	mux := http.NewServeMux()
	mux.HandleFunc("/call/", b.call)
	mux.HandleFunc("/events", b.events)
	mux.HandleFunc("/quit", func(w http.ResponseWriter, _ *http.Request) { stop(); w.WriteHeader(http.StatusNoContent) })
	return cors(mux)
}

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (b *bridge) emit(name string, data any) {
	raw, err := json.Marshal(map[string]any{"name": name, "data": data})
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.subs {
		select {
		case c <- raw:
		default: // a stuck page misses events rather than holding the node up
		}
	}
}

func (b *bridge) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	c := make(chan []byte, 256)
	b.mu.Lock()
	b.subs[c] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.subs, c)
		b.mu.Unlock()
	}()
	fl.Flush()
	for {
		select {
		case raw := <-c:
			fmt.Fprintf(w, "data: %s\n\n", raw)
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// call runs a desktop method: POST /call/<Name> with a JSON array of its arguments; the answer is
// {"result": …} or {"error": "…"}.
func (b *bridge) call(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	reply := func(res any, err error) {
		out := map[string]any{"result": res}
		if err != nil {
			out = map[string]any{"error": err.Error()}
		}
		_ = json.NewEncoder(w).Encode(out)
	}
	name := strings.TrimPrefix(r.URL.Path, "/call/")
	target, ok := bridged[name]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		reply(nil, errors.New(name+" is not bridged"))
		return
	}
	m := b.api.MethodByName(target)
	var raw []json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil && err.Error() != "EOF" {
		reply(nil, err)
		return
	}
	ft := m.Type()
	if len(raw) < ft.NumIn() {
		reply(nil, fmt.Errorf("%s takes %d arguments", name, ft.NumIn()))
		return
	}
	args := make([]reflect.Value, ft.NumIn())
	for i := range args {
		p := reflect.New(ft.In(i))
		if err := json.Unmarshal(raw[i], p.Interface()); err != nil {
			reply(nil, fmt.Errorf("argument %d: %w", i, err))
			return
		}
		args[i] = p.Elem()
	}
	outs := m.Call(args)
	var res any
	var err error
	for _, o := range outs {
		if e, ok := o.Interface().(error); ok || (o.Kind() == reflect.Interface && o.IsNil() && o.Type().Implements(reflect.TypeOf((*error)(nil)).Elem())) {
			err = e
			continue
		}
		res = o.Interface()
	}
	reply(res, err)
}
