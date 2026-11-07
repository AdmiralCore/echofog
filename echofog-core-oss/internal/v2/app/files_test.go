package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/files"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/voice"
	"github.com/echofog/echofog-core/internal/v2/voice/voicetest"
	"google.golang.org/protobuf/proto"
)

// ---- doubles ------------------------------------------------------------------------------------------------------

type tclock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *tclock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *tclock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// holder records what the app gave to files.Server.
type holder struct {
	mu      sync.Mutex
	added   []files.Share
	removed [][16]byte
}

func (h *holder) Add(s files.Share) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.added = append(h.added, s)
	return nil
}

func (h *holder) Remove(fid [16]byte) {
	h.mu.Lock()
	h.removed = append(h.removed, fid)
	h.mu.Unlock()
}

func (h *holder) share(fid [16]byte) (files.Share, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.added {
		if s.Offer.FID == fid {
			return s, true
		}
	}
	return files.Share{}, false
}

func (h *holder) wasRemoved(fid [16]byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.removed {
		if f == fid {
			return true
		}
	}
	return false
}

type sent struct {
	to [32]byte
	m  *chatpb.PersonalMessageProto
}

// sendLog is the chat service of a node: it keeps what was sent.
type sendLog struct {
	mu   sync.Mutex
	msgs []sent
	fail bool
}

func (l *sendLog) Send(_ context.Context, to [32]byte, m *chatpb.PersonalMessageProto) ([16]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail {
		return [16]byte{}, errors.New("unreachable")
	}
	l.msgs = append(l.msgs, sent{to, m})
	return [16]byte{1}, nil
}

func (l *sendLog) of(t chatpb.PersonalMessageType, to [32]byte) []*chatpb.PersonalMessageProto {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*chatpb.PersonalMessageProto
	for _, s := range l.msgs {
		if s.m.Type == t && s.to == to {
			out = append(out, s.m)
		}
	}
	return out
}

type world struct {
	mu    sync.Mutex
	nodes map[[32]byte]*fileNode
}

func (w *world) get(n [32]byte) *fileNode {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nodes[n]
}

// fakeFiles is files.Fetch without a network: it reads the chunks the sender's share offers, puts them
// in the real FileStore the app opened, and commits (the real proof against the root).
type fakeFiles struct {
	w    *world
	self [32]byte

	mu        sync.Mutex
	calls     int
	inflight  int
	puts      int
	failAfter int           // chunks to fetch before the sender "goes away"; 0: never
	block     chan struct{} // when set, Fetch waits for it (or for its context)
	confirm   bool          // tell the sender that the file arrived, as files.Fetch does
	err       error
}

func (f *fakeFiles) Fetch(ctx context.Context, from [32]byte, o files.Offer, st files.Store, opt files.FetchOptions) error {
	f.mu.Lock()
	f.calls++
	f.inflight++
	block, fail, ferr := f.block, f.failAfter, f.err
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inflight--; f.mu.Unlock() }()
	if ferr != nil {
		return ferr
	}
	src := f.w.get(from)
	if src == nil {
		return files.ErrUnavailable
	}
	sh, ok := src.hold.share(o.FID)
	if !ok {
		return files.ErrUnavailable
	}
	allowed := false
	for _, r := range sh.Recipients {
		allowed = allowed || r == f.self
	}
	if !allowed {
		return files.ErrUnavailable
	}
	if block != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-block:
		}
	}
	n, put := o.Chunks(), 0
	for i := 0; i < n; i++ {
		if st.Have(i) {
			continue
		}
		if fail > 0 && put >= fail {
			f.mu.Lock()
			f.failAfter = 0
			f.mu.Unlock()
			return fmt.Errorf("%w: the sender went away", files.ErrUnavailable)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		buf := make([]byte, min(int64(files.ChunkSize), int64(o.Size)-int64(i)*files.ChunkSize))
		if _, err := sh.Source.ReadAt(buf, int64(i)*files.ChunkSize); err != nil {
			return err
		}
		if err := st.Put(i, buf); err != nil {
			return err
		}
		put++
		f.mu.Lock()
		f.puts++
		f.mu.Unlock()
		if opt.OnProgress != nil {
			opt.OnProgress(st.Count(), n)
		}
	}
	if err := st.Commit(); err != nil {
		return err
	}
	f.mu.Lock()
	confirm := f.confirm
	f.mu.Unlock()
	if confirm {
		src.app.FileServed(o.FID, f.self)
	}
	return nil
}

func (f *fakeFiles) stats() (calls, inflight, puts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.inflight, f.puts
}

type fileNode struct {
	t    *testing.T
	w    *world
	node [32]byte
	rig  *rig
	app  *app.App
	hold *holder
	log  *sendLog
	net  *fakeFiles
	clk  *tclock
	dir  string // downloads
	src  string // where the files to send are
	lim  app.FileLimits

	mu    sync.Mutex
	free  uint64
	retry chan struct{} // when set, a retry waits for it
	waits int
}

func newWorld() *world { return &world{nodes: map[[32]byte]*fileNode{}} }

func (w *world) node(t *testing.T, id byte, lim app.FileLimits) *fileNode {
	t.Helper()
	n := &fileNode{t: t, w: w, node: nid(id), rig: newRig(t, app.ModePublic), hold: &holder{}, log: &sendLog{},
		clk: &tclock{t: testNow}, dir: t.TempDir(), src: t.TempDir(), lim: lim, free: 1 << 40}
	n.net = &fakeFiles{w: w, self: n.node, confirm: true}
	w.mu.Lock()
	w.nodes[n.node] = n
	w.mu.Unlock()
	n.build()
	return n
}

// build makes a fresh app over the node's store and download directory: a restart.
func (n *fileNode) build() {
	n.t.Helper()
	a, err := app.New(app.Config{
		Runtime: n.rig.rt, Links: n.rig.ln, Store: n.rig.kv, Clock: n.clk, Self: n.node, Mode: app.ModePublic, Version: "0.3.0",
		Chat: n.log, Files: n.hold, FileNet: n.net, FileDir: n.dir, FileLimits: n.lim,
		FileFree: func(string) (uint64, error) { n.mu.Lock(); defer n.mu.Unlock(); return n.free, nil },
		FileWait: func(ctx context.Context, _ time.Duration) bool {
			n.mu.Lock()
			n.waits++
			ch := n.retry
			n.mu.Unlock()
			if ch == nil {
				return ctx.Err() == nil
			}
			select {
			case <-ctx.Done():
				return false
			case <-ch:
				return true
			}
		},
	})
	if err != nil {
		n.t.Fatalf("New: %v", err)
	}
	n.app = a
	n.t.Cleanup(func() { _ = a.Stop(context.Background()) })
}

func (n *fileNode) restart() {
	n.t.Helper()
	if err := n.app.Stop(context.Background()); err != nil {
		n.t.Fatal(err)
	}
	n.build()
	if err := n.app.Start(context.Background()); err != nil {
		n.t.Fatal(err)
	}
}

func (n *fileNode) hex() string { return hex.EncodeToString(n.node[:]) }

func (n *fileNode) befriend(o *fileNode) {
	n.t.Helper()
	url := "echofog://join?v=2&card=" + hex.EncodeToString(o.node[:4])
	n.rig.ln.preview[url] = &links.Preview{Kind: links.KindContact, Node: o.node, Device: o.node, NotAfter: testNow.Add(time.Hour)}
	n.rig.ln.peer = node.Peer{Node: node.NodeID(o.node), Device: node.DeviceID(o.node)}
	if _, err := n.app.AddByLink(context.Background(), url); err != nil {
		n.t.Fatalf("AddByLink: %v", err)
	}
}

// file writes size bytes of random data and returns the path and the data.
func (n *fileNode) file(name string, size int) (string, []byte) {
	n.t.Helper()
	data := make([]byte, size)
	_, _ = rand.Read(data)
	p := filepath.Join(n.src, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		n.t.Fatal(err)
	}
	return p, data
}

func until(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 2500; i++ {
		if f() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func (n *fileNode) waitState(id, state string) app.Transfer {
	n.t.Helper()
	var last app.Transfer
	until(n.t, "transfer "+id[:8]+" to be "+state, func() bool {
		tr, err := n.app.GetFileTransfer(id)
		last = tr
		return err == nil && tr.State == state
	})
	return last
}

func (n *fileNode) state(id string) app.Transfer {
	n.t.Helper()
	tr, err := n.app.GetFileTransfer(id)
	if err != nil {
		n.t.Fatalf("GetFileTransfer: %v", err)
	}
	return tr
}

// pair makes two nodes that are each other's contacts.
func pair(t *testing.T, lim app.FileLimits) (*fileNode, *fileNode) {
	t.Helper()
	w := newWorld()
	a, b := w.node(t, 11, lim), w.node(t, 12, lim)
	a.befriend(b)
	b.befriend(a)
	return a, b
}

// offerTo carries the offer a sent to b over the chat and hands it to b's app.
func offerTo(t *testing.T, a, b *fileNode) error {
	t.Helper()
	ms := a.log.of(chatpb.PersonalMessageType_MSG_TYPE_FILE, b.node)
	if len(ms) == 0 {
		t.Fatal("no file message was sent")
	}
	return b.app.OnFileOffer(a.node, "", ms[len(ms)-1])
}

func offerMessage(t *testing.T, sender [32]byte, data []byte, name string) (*chatpb.PersonalMessageProto, files.Offer) {
	t.Helper()
	sh, err := files.PrepareShare(bytes.NewReader(data), int64(len(data)), name, "", sender, [][32]byte{nid(99)})
	if err != nil {
		t.Fatal(err)
	}
	return sh.Offer.Message(), sh.Offer
}

func idOf(o files.Offer) string { return hex.EncodeToString(o.FID[:]) }

const chunk = files.ChunkSize

// ---- sending ------------------------------------------------------------------------------------------------------

func TestSendFileOffersAndDoesNotClaimDelivery(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, data := a.file("отчёт.pdf", 3*chunk/2)
	tr, err := a.app.SendFile(context.Background(), b.hex(), path)
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	if tr.Dir != app.DirOut || tr.State != app.FileOffered || tr.Peer != b.hex() || tr.Size != int64(len(data)) ||
		tr.Name != "отчёт.pdf" || tr.Path != path || tr.Done != 0 {
		t.Fatalf("transfer %+v", tr)
	}
	if len(a.hold.added) != 1 {
		t.Fatalf("%d shares", len(a.hold.added))
	}
	o := a.hold.added[0].Offer
	if o.Sender != a.node || o.Size != uint64(len(data)) || a.hold.added[0].Recipients[0] != b.node || idOf(o) != tr.ID {
		t.Fatalf("offer %+v", o)
	}
	ms := a.log.of(chatpb.PersonalMessageType_MSG_TYPE_FILE, b.node)
	if len(ms) != 1 {
		t.Fatalf("%d offer messages", len(ms))
	}
	if got, err := files.OfferFrom(ms[0]); err != nil || got != o {
		t.Fatalf("the message carries another offer: %v", err)
	}
	// queued or not, an offer is not a delivery
	if st := a.state(tr.ID); st.State != app.FileOffered {
		t.Fatalf("state %q", st.State)
	}
}

func TestSendFileRefusals(t *testing.T) {
	a, b := pair(t, app.FileLimits{MaxFileSize: 1000})
	ctx := context.Background()
	small, _ := a.file("s.bin", 500)
	big, _ := a.file("b.bin", 2000)
	stranger := hx(nid(77))
	cases := []struct {
		name string
		peer string
		path string
		want error
	}{
		{"not a contact", stranger, small, app.ErrNoContact},
		{"a directory", b.hex(), a.src, app.ErrBadFile},
		{"missing", b.hex(), filepath.Join(a.src, "nope"), app.ErrBadFile},
		{"over the limit", b.hex(), big, app.ErrFileTooLarge},
		{"bad peer id", "zz", small, app.ErrNoContact},
	}
	for _, c := range cases {
		if _, err := a.app.SendFile(ctx, c.peer, c.path); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if err := a.app.Block(b.hex()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.app.SendFile(ctx, b.hex(), small); !errors.Is(err, app.ErrBlocked) {
		t.Errorf("blocked: %v", err)
	}
	_ = a.app.Unblock(b.hex())
	// the chat cannot take the offer: nothing stays shared
	a.log.fail = true
	if _, err := a.app.SendFile(ctx, b.hex(), small); err == nil {
		t.Error("an offer that could not be sent was accepted")
	}
	a.log.fail = false
	if list, _ := a.app.ListFileTransfers(""); len(list) != 0 {
		t.Errorf("refused sends left %d transfers", len(list))
	}
	for _, s := range a.hold.added {
		if !a.hold.wasRemoved(s.Offer.FID) {
			t.Error("a share outlived its refused offer")
		}
	}
}

func TestFilesOff(t *testing.T) {
	r := newRig(t, app.ModePublic)
	if _, err := r.a.SendFile(context.Background(), hx(nid(7)), "x"); !errors.Is(err, app.ErrFilesOff) {
		t.Fatalf("SendFile: %v", err)
	}
	if _, err := r.a.AcceptFile("00"); !errors.Is(err, app.ErrFilesOff) {
		t.Fatalf("AcceptFile: %v", err)
	}
}

func TestOutgoingOfferExpires(t *testing.T) {
	a, b := pair(t, app.FileLimits{OfferTTL: time.Hour})
	path, _ := a.file("x.bin", 1000)
	tr, err := a.app.SendFile(context.Background(), b.hex(), path)
	if err != nil {
		t.Fatal(err)
	}
	a.clk.Advance(2 * time.Hour)
	got := a.state(tr.ID)
	if got.State != app.FileFailed || got.Error == "" || !a.hold.wasRemoved(a.hold.added[0].Offer.FID) {
		t.Fatalf("an offer nobody took: %+v", got)
	}
	a.app.FileServed(a.hold.added[0].Offer.FID, b.node) // late: ignored
	if a.state(tr.ID).State != app.FileFailed {
		t.Fatal("a late confirmation revived an expired offer")
	}
}

func TestGroupFileCountsRecipients(t *testing.T) {
	w := newWorld()
	a, b, c := w.node(t, 11, app.FileLimits{}), w.node(t, 12, app.FileLimits{}), w.node(t, 13, app.FileLimits{})
	path, _ := a.file("g.bin", 2000)
	tr, err := a.app.SendGroupFile(context.Background(), "grp1", [][32]byte{b.node, c.node}, path)
	if err != nil {
		t.Fatalf("SendGroupFile: %v", err)
	}
	if tr.Group != "grp1" || tr.Members != 2 || tr.Delivered != 0 || tr.Peer != "" || tr.State != app.FileOffered {
		t.Fatalf("transfer %+v", tr)
	}
	for _, m := range []*fileNode{b, c} {
		if len(a.log.of(chatpb.PersonalMessageType_MSG_TYPE_FILE, m.node)) != 1 {
			t.Fatalf("member %x got no offer", m.node[:2])
		}
	}
	fid := a.hold.added[0].Offer.FID
	a.app.FileServed(fid, b.node)
	a.app.FileServed(fid, b.node)  // twice: one
	a.app.FileServed(fid, nid(90)) // not a member
	if got := a.state(tr.ID); got.Delivered != 1 || got.State != app.FileOffered {
		t.Fatalf("after one member: %+v", got)
	}
	a.app.FileServed(fid, c.node)
	if got := a.state(tr.ID); got.Delivered != 2 || got.State != app.FileDone || got.Done != got.Size {
		t.Fatalf("after both: %+v", got)
	}
	if _, err := a.app.SendGroupFile(context.Background(), "grp1", nil, path); err == nil {
		t.Fatal("a group file with no members")
	}
}

// ---- receiving ----------------------------------------------------------------------------------------------------

func TestIncomingOfferWaitsForTheUser(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, _ := a.file("фото.jpg", 2*chunk)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	if err := offerTo(t, a, b); err != nil {
		t.Fatalf("OnFileOffer: %v", err)
	}
	if err := offerTo(t, a, b); err != nil { // the same message again
		t.Fatalf("a repeated offer: %v", err)
	}
	list, err := b.app.ListFileTransfers("")
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v, %v", list, err)
	}
	in := list[0]
	if in.ID != tr.ID || in.Dir != app.DirIn || in.State != app.FileOffered || in.Peer != a.hex() || in.Name != "фото.jpg" || in.Size != 2*chunk || in.Path != "" {
		t.Fatalf("incoming %+v", in)
	}
	if calls, _, _ := b.net.stats(); calls != 0 {
		t.Fatal("a file was fetched before the user accepted it")
	}
	if es, _ := os.ReadDir(b.dir); len(es) != 0 {
		t.Fatalf("%d files on disk before accepting", len(es))
	}
	if got, _ := b.app.ListFileTransfers(hx(nid(55))); len(got) != 0 {
		t.Fatal("the filter by peer returned another peer's files")
	}
	if got, _ := b.app.ListFileTransfers(a.hex()); len(got) != 1 {
		t.Fatal("the filter by peer lost the file")
	}
}

func TestIncomingOfferRefusals(t *testing.T) {
	w := newWorld()
	b := w.node(t, 12, app.FileLimits{MaxFileSize: 5000, MaxPerSender: 2})
	a, c := w.node(t, 11, app.FileLimits{}), w.node(t, 13, app.FileLimits{})
	b.befriend(a)
	b.befriend(c)
	data := bytes.Repeat([]byte{7}, 3000)
	good, o := offerMessage(t, a.node, data, "a.bin")
	if err := b.app.OnFileOffer(a.node, "", good); err != nil {
		t.Fatal(err)
	}

	forged, _ := offerMessage(t, a.node, data, "b.bin") // names a as the sender, arrives from c
	otherRoot := good
	_ = otherRoot
	o2 := o
	o2.Root = [32]byte{9: 9} // the same file ID, another root
	sameID := o2.Message()
	huge, _ := offerMessage(t, a.node, make([]byte, 6000), "h.bin")
	text := proto3clone(good)
	text.Type = chatpb.PersonalMessageType_MSG_TYPE_TEXT
	junk := proto3clone(good)
	junk.Metadata = base64.RawURLEncoding.EncodeToString([]byte("not an offer"))

	cases := []struct {
		name string
		from [32]byte
		m    *chatpb.PersonalMessageProto
	}{
		{"offer names another sender", c.node, forged},
		{"stranger", nid(66), good},
		{"same id with another root", a.node, sameID},
		{"over the size limit", a.node, huge},
		{"not a file message", a.node, text},
		{"garbage offer", a.node, junk},
		{"nil", a.node, nil},
	}
	for _, c := range cases {
		if err := b.app.OnFileOffer(c.from, "", c.m); !errors.Is(err, app.ErrBadOffer) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if err := b.app.Block(a.hex()); err != nil {
		t.Fatal(err)
	}
	if err := b.app.OnFileOffer(a.node, "", offerOf(t, a.node, 1)); !errors.Is(err, app.ErrBadOffer) {
		t.Errorf("blocked contact: %v", err)
	}
	_ = b.app.Unblock(a.hex())
	// at most MaxPerSender offers wait from one contact
	if err := b.app.OnFileOffer(a.node, "", offerOf(t, a.node, 2)); err != nil {
		t.Fatalf("second offer: %v", err)
	}
	if err := b.app.OnFileOffer(a.node, "", offerOf(t, a.node, 3)); !errors.Is(err, app.ErrTooManyTransfers) {
		t.Errorf("third offer: %v", err)
	}
	if list, _ := b.app.ListFileTransfers(""); len(list) != 2 {
		t.Fatalf("%d transfers after the refusals, want 2", len(list))
	}
	if tr := b.state(idOf(o)); tr.Name != "a.bin" || tr.Size != 3000 {
		t.Fatalf("the first offer was overwritten: %+v", tr)
	}
}

func offerOf(t *testing.T, sender [32]byte, n byte) *chatpb.PersonalMessageProto {
	m, _ := offerMessage(t, sender, bytes.Repeat([]byte{n}, 100+int(n)), fmt.Sprintf("f%d.bin", n))
	return m
}

func proto3clone(m *chatpb.PersonalMessageProto) *chatpb.PersonalMessageProto {
	return proto.Clone(m).(*chatpb.PersonalMessageProto)
}

func TestGroupOfferFromAMemberWhoIsNoContact(t *testing.T) {
	w := newWorld()
	b := w.node(t, 12, app.FileLimits{})
	a := w.node(t, 11, app.FileLimits{})
	m, _ := offerMessage(t, a.node, bytes.Repeat([]byte{1}, 500), "g.bin")
	// the group layer vouches for the membership; a direct offer from a stranger stays refused
	if err := b.app.OnFileOffer(a.node, "", m); !errors.Is(err, app.ErrBadOffer) {
		t.Fatalf("direct: %v", err)
	}
	if err := b.app.OnFileOffer(a.node, "grp1", m); err != nil {
		t.Fatalf("group: %v", err)
	}
	list, _ := b.app.ListFileTransfers("")
	if len(list) != 1 || list[0].Group != "grp1" || list[0].State != app.FileOffered {
		t.Fatalf("list %+v", list)
	}
}

func TestAcceptDownloadsAndSenderLearnsOnlyFromTheConfirmation(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	b.net.confirm = false // the sender has not been told yet
	path, data := a.file("архив.zip", 5*chunk+1234)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	if err := offerTo(t, a, b); err != nil {
		t.Fatal(err)
	}
	got, err := b.app.AcceptFile(tr.ID)
	if err != nil || got.State != app.FileActive {
		t.Fatalf("AcceptFile: %+v, %v", got, err)
	}
	done := b.waitState(tr.ID, app.FileDone)
	if done.Done != int64(len(data)) || done.Path == "" || done.Stalled || filepath.Dir(done.Path) != b.dir {
		t.Fatalf("done %+v", done)
	}
	if disk, err := os.ReadFile(done.Path); err != nil || !bytes.Equal(disk, data) {
		t.Fatalf("the received file differs: %v", err)
	}
	if p, err := b.app.FileTransferPath(tr.ID); err != nil || p != done.Path {
		t.Fatalf("FileTransferPath: %q, %v", p, err)
	}
	if b.app.DownloadDir() != b.dir {
		t.Fatalf("DownloadDir %q", b.app.DownloadDir())
	}
	// A was not told: still an offer, never "done" or "delivered"
	if s := a.state(tr.ID); s.State != app.FileOffered || s.Done != 0 {
		t.Fatalf("the sender claims %+v before any confirmation", s)
	}
	fid := a.hold.added[0].Offer.FID
	a.app.FileServed(fid, nid(90)) // not a recipient
	if s := a.state(tr.ID); s.State != app.FileOffered {
		t.Fatal("a stranger's confirmation counted")
	}
	a.app.FileServed(fid, b.node)
	if s := a.state(tr.ID); s.State != app.FileDone || s.Done != s.Size {
		t.Fatalf("after the confirmation: %+v", s)
	}
}

func TestPathOnlyOfFinishedFiles(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, _ := a.file("p.bin", 100)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	_ = offerTo(t, a, b)
	if _, err := b.app.FileTransferPath(tr.ID); !errors.Is(err, app.ErrBadState) {
		t.Fatalf("path of an offered file: %v", err)
	}
	if _, err := b.app.FileTransferPath("0000"); !errors.Is(err, app.ErrNoTransfer) {
		t.Fatalf("path of nothing: %v", err)
	}
	if _, err := b.app.GetFileTransfer("0000"); !errors.Is(err, app.ErrNoTransfer) {
		t.Fatalf("get nothing: %v", err)
	}
	if _, err := b.app.AcceptFile("0000"); !errors.Is(err, app.ErrNoTransfer) {
		t.Fatalf("accept nothing: %v", err)
	}
}

// ---- resume, cancel, limits ---------------------------------------------------------------------------------------

func TestFetchResumesWhereTheSenderLeftOff(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, data := a.file("big.bin", 6*chunk)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	_ = offerTo(t, a, b)
	b.net.failAfter = 3
	b.retry = make(chan struct{}) // the retry waits until released
	if _, err := b.app.AcceptFile(tr.ID); err != nil {
		t.Fatal(err)
	}
	st := waitFor(t, b, tr.ID, func(x app.Transfer) bool { return x.Stalled })
	if st.State != app.FileActive || st.Done != 3*chunk || st.Error != "" {
		t.Fatalf("stalled transfer %+v", st)
	}
	close(b.retry)
	done := b.waitState(tr.ID, app.FileDone)
	if disk, _ := os.ReadFile(done.Path); !bytes.Equal(disk, data) {
		t.Fatal("the resumed file differs")
	}
	if _, _, puts := b.net.stats(); puts != 6 {
		t.Fatalf("%d chunks fetched in all, want 6: the resume fetched some twice", puts)
	}
}

func waitFor(t *testing.T, n *fileNode, id string, f func(app.Transfer) bool) app.Transfer {
	t.Helper()
	var last app.Transfer
	until(t, "transfer condition", func() bool {
		tr, err := n.app.GetFileTransfer(id)
		last = tr
		return err == nil && f(tr)
	})
	return last
}

func TestDownloadSurvivesARestart(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, data := a.file("big.bin", 6*chunk)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	_ = offerTo(t, a, b)
	if err := b.app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.net.failAfter = 3
	b.retry = make(chan struct{}) // never released: the app is stopped while it waits
	if _, err := b.app.AcceptFile(tr.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, b, tr.ID, func(x app.Transfer) bool { return x.Stalled })
	b.mu.Lock()
	b.retry = nil
	b.mu.Unlock()
	b.restart()
	if _, inflight, _ := b.net.stats(); inflight != 0 {
		t.Fatalf("%d fetches outlived Stop", inflight)
	}
	got := b.state(tr.ID)
	if got.State != app.FileActive || !got.Stalled || got.Dir != app.DirIn || got.Name != "big.bin" {
		t.Fatalf("after the restart %+v", got)
	}
	if _, err := b.app.AcceptFile(tr.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	done := b.waitState(tr.ID, app.FileDone)
	if disk, _ := os.ReadFile(done.Path); !bytes.Equal(disk, data) {
		t.Fatal("the file differs after the resume")
	}
	if _, _, puts := b.net.stats(); puts != 6 {
		t.Fatalf("%d chunks fetched across the restart, want 6", puts)
	}
}

func TestOutgoingOffersDoNotSurviveARestart(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, _ := a.file("x.bin", 100)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	_ = a.app.Start(context.Background())
	a.restart()
	if got := a.state(tr.ID); got.State != app.FileFailed || got.Error == "" {
		t.Fatalf("an offer whose source went with the process: %+v", got)
	}
}

func TestCancelStopsTheFetchAndDropsThePartialFile(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, _ := a.file("c.bin", 4*chunk)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	_ = offerTo(t, a, b)
	b.net.block = make(chan struct{})
	if _, err := b.app.AcceptFile(tr.ID); err != nil {
		t.Fatal(err)
	}
	until(t, "the fetch to start", func() bool { _, in, _ := b.net.stats(); return in == 1 })
	if err := b.app.CancelFile(tr.ID); err != nil {
		t.Fatalf("CancelFile: %v", err)
	}
	until(t, "the fetch to end", func() bool { _, in, _ := b.net.stats(); return in == 0 })
	if got := b.state(tr.ID); got.State != app.FileCancelled || got.Path != "" {
		t.Fatalf("after cancel %+v", got)
	}
	if es, _ := os.ReadDir(b.dir); len(es) != 0 {
		t.Fatalf("%d files left after cancel", len(es))
	}
	if _, err := b.app.AcceptFile(tr.ID); !errors.Is(err, app.ErrBadState) {
		t.Fatalf("accept after cancel: %v", err)
	}
	if err := b.app.CancelFile(tr.ID); !errors.Is(err, app.ErrBadState) {
		t.Fatalf("cancel twice: %v", err)
	}
}

func TestCancelOutgoingStopsSharing(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, _ := a.file("o.bin", 1000)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	if err := a.app.CancelFile(tr.ID); err != nil {
		t.Fatal(err)
	}
	if got := a.state(tr.ID); got.State != app.FileCancelled || !a.hold.wasRemoved(a.hold.added[0].Offer.FID) {
		t.Fatalf("after cancel %+v", got)
	}
	// a confirmation for a cancelled offer changes nothing
	a.app.FileServed(a.hold.added[0].Offer.FID, b.node)
	if a.state(tr.ID).State != app.FileCancelled {
		t.Fatal("a cancelled offer became done")
	}
}

func TestDecline(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, _ := a.file("d.bin", 1000)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	_ = offerTo(t, a, b)
	if err := b.app.DeclineFile(tr.ID); err != nil {
		t.Fatal(err)
	}
	if got := b.state(tr.ID); got.State != app.FileDeclined {
		t.Fatalf("state %q", got.State)
	}
	if _, err := b.app.AcceptFile(tr.ID); !errors.Is(err, app.ErrBadState) {
		t.Fatalf("accept after decline: %v", err)
	}
	if err := b.app.DeclineFile(tr.ID); !errors.Is(err, app.ErrBadState) {
		t.Fatalf("decline twice: %v", err)
	}
	if calls, _, _ := b.net.stats(); calls != 0 {
		t.Fatal("a declined file was fetched")
	}
}

func TestOnlyMaxActiveFetchesRun(t *testing.T) {
	a, b := pair(t, app.FileLimits{MaxActive: 2})
	b.net.block = make(chan struct{})
	var ids []string
	for i := 0; i < 3; i++ {
		path, _ := a.file(fmt.Sprintf("m%d.bin", i), 1000+i)
		tr, err := a.app.SendFile(context.Background(), b.hex(), path)
		if err != nil {
			t.Fatal(err)
		}
		if err := offerTo(t, a, b); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tr.ID)
	}
	for _, id := range ids[:2] {
		if _, err := b.app.AcceptFile(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.app.AcceptFile(ids[2]); !errors.Is(err, app.ErrTooManyTransfers) {
		t.Fatalf("third fetch: %v", err)
	}
	if got := b.state(ids[2]); got.State != app.FileOffered {
		t.Fatalf("a refused accept changed the state to %q", got.State)
	}
	if err := b.app.CancelFile(ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := b.app.AcceptFile(ids[2]); err != nil {
		t.Fatalf("after a slot freed: %v", err)
	}
	close(b.net.block)
	b.waitState(ids[1], app.FileDone)
	b.waitState(ids[2], app.FileDone)
}

func TestAcceptNeedsRoomOnTheDisk(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, _ := a.file("full.bin", 4*chunk)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	_ = offerTo(t, a, b)
	b.mu.Lock()
	b.free = 1000
	b.mu.Unlock()
	if _, err := b.app.AcceptFile(tr.ID); !errors.Is(err, files.ErrDisk) {
		t.Fatalf("AcceptFile on a full disk: %v", err)
	}
	if got := b.state(tr.ID); got.State != app.FileOffered {
		t.Fatalf("state %q: the user can free space and retry", got.State)
	}
	if calls, _, _ := b.net.stats(); calls != 0 {
		t.Fatal("fetch started without room")
	}
}

func TestFailedFetchIsReportedWithoutLosingTheState(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, _ := a.file("f.bin", 2*chunk)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	_ = offerTo(t, a, b)
	b.net.err = files.ErrCorrupt
	if _, err := b.app.AcceptFile(tr.ID); err != nil {
		t.Fatal(err)
	}
	got := b.waitState(tr.ID, app.FileFailed)
	if got.Error == "" || got.Path != "" || got.Stalled {
		t.Fatalf("failed transfer %+v", got)
	}
}

// ---- events -------------------------------------------------------------------------------------------------------

func TestTransferEvents(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	var mu sync.Mutex
	var seen []app.Transfer
	stop := b.app.OnFileTransfer(func(tr app.Transfer) { mu.Lock(); seen = append(seen, tr); mu.Unlock() })
	path, _ := a.file("e.bin", 3*chunk)
	tr, _ := a.app.SendFile(context.Background(), b.hex(), path)
	_ = offerTo(t, a, b)
	_, _ = b.app.AcceptFile(tr.ID)
	b.waitState(tr.ID, app.FileDone)
	stop()
	mu.Lock()
	n := len(seen)
	states := []string{}
	progress := false
	for _, s := range seen {
		states = append(states, s.State)
		progress = progress || (s.State == app.FileActive && s.Done > 0 && s.Done < s.Size)
	}
	mu.Unlock()
	if len(states) < 3 || states[0] != app.FileOffered || states[len(states)-1] != app.FileDone {
		t.Fatalf("events %v", states)
	}
	if !progress {
		t.Fatalf("no progress event: %v", states)
	}
	path2, _ := a.file("e2.bin", 100)
	tr2, _ := a.app.SendFile(context.Background(), b.hex(), path2)
	_ = offerTo(t, a, b)
	_ = tr2
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Fatal("events after the unsubscribe")
	}
}

// ---- concurrency --------------------------------------------------------------------------------------------------

func TestConcurrentFileCalls(t *testing.T) {
	a, b := pair(t, app.FileLimits{MaxActive: 4})
	var ids []string
	for i := 0; i < 12; i++ {
		path, _ := a.file(fmt.Sprintf("r%d.bin", i), chunk+i)
		tr, err := a.app.SendFile(context.Background(), b.hex(), path)
		if err != nil {
			t.Fatal(err)
		}
		if err := offerTo(t, a, b); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tr.ID)
	}
	stop := b.app.OnFileTransfer(func(app.Transfer) {})
	defer stop()
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				for k := 0; k < 50; k++ {
					if _, err := b.app.AcceptFile(id); err == nil || !errors.Is(err, app.ErrTooManyTransfers) {
						break
					}
					time.Sleep(time.Millisecond)
				}
			case 1:
				_ = b.app.DeclineFile(id)
			default:
				_, _ = b.app.AcceptFile(id)
				_ = b.app.CancelFile(id)
			}
			_, _ = b.app.ListFileTransfers("")
			_, _ = b.app.GetFileTransfer(id)
		}()
	}
	wg.Wait()
	until(t, "every fetch to end", func() bool { _, in, _ := b.net.stats(); return in == 0 })
	for _, id := range ids {
		switch s := b.state(id).State; s {
		case app.FileDone, app.FileDeclined, app.FileCancelled, app.FileActive, app.FileOffered:
		default:
			t.Fatalf("transfer in state %q", s)
		}
	}
}

// ---- voice messages -----------------------------------------------------------------------------------------------

func recording(secs int) voice.Recording {
	return voice.Recording{Ogg: voicetest.Clip(secs), Levels: bytes.Repeat([]byte{10, 40, 90}, 20)}
}

func voiceTo(t *testing.T, a, b *fileNode) *chatpb.PersonalMessageProto {
	t.Helper()
	ms := a.log.of(chatpb.PersonalMessageType_MSG_TYPE_VOICE, b.node)
	if len(ms) == 0 {
		t.Fatal("no voice message was sent")
	}
	return ms[len(ms)-1]
}

func TestShortVoiceMessageTravelsInTheMessage(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	res, err := a.app.SendVoice(context.Background(), b.hex(), recording(5))
	if err != nil || res.ID == "" || res.Transfer != nil {
		t.Fatalf("SendVoice: %+v, %v", res, err)
	}
	if len(a.hold.added) != 0 {
		t.Fatal("a short clip became a file")
	}
	m := voiceTo(t, a, b)
	id, err := b.app.OnVoice(a.node, m)
	if err != nil || id != res.ID {
		t.Fatalf("OnVoice: %q, %v", id, err)
	}
	clip, ok := b.app.VoiceAudio(id)
	if !ok || len(clip) == 0 || bytes.Contains(clip, []byte("libopus")) || bytes.Contains(clip, []byte("Pixel")) {
		t.Fatalf("clip ok=%v len=%d: the player must get a clean clip", ok, len(clip))
	}
	if again, err := b.app.OnVoice(a.node, m); err != nil || again != id {
		t.Fatalf("the same message again: %q, %v", again, err)
	}
	// someone else's message under a's name, or a stranger's, is refused
	c := b.w.node(t, 13, app.FileLimits{})
	b.befriend(c)
	if _, err := b.app.OnVoice(c.node, m); err == nil {
		t.Fatal("a message that names another sender was accepted")
	}
	if _, err := b.app.OnVoice(nid(66), m); err == nil {
		t.Fatal("a voice message from a stranger was accepted")
	}
	if _, ok := b.app.VoiceAudio("nope"); ok {
		t.Fatal("a clip for an unknown id")
	}
}

func TestLongVoiceMessageIsFetchedAsAFileAndLeavesNoFile(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	res, err := a.app.SendVoice(context.Background(), b.hex(), recording(40))
	if err != nil || res.Transfer == nil || !res.Transfer.Voice {
		t.Fatalf("SendVoice: %+v, %v", res, err)
	}
	if len(a.hold.added) != 1 || a.hold.added[0].Offer.Mime != "audio/ogg" || a.hold.added[0].Offer.Name != "voice.ogg" {
		t.Fatalf("shares %+v", a.hold.added)
	}
	id, err := b.app.OnVoice(a.node, voiceTo(t, a, b))
	if err != nil {
		t.Fatalf("OnVoice: %v", err)
	}
	until(t, "the clip", func() bool { _, ok := b.app.VoiceAudio(id); return ok })
	clip, _ := b.app.VoiceAudio(id)
	if bytes.Contains(clip, []byte("libopus")) {
		t.Fatal("the sender's tags reached the player")
	}
	tr := b.waitState(res.Transfer.ID, app.FileDone)
	if !tr.Voice || tr.Path != "" {
		t.Fatalf("voice transfer %+v", tr)
	}
	if es, _ := os.ReadDir(b.dir); len(es) != 0 {
		names := []string{}
		for _, e := range es {
			names = append(names, e.Name())
		}
		t.Fatalf("a voice clip stayed in the downloads: %s", strings.Join(names, ", "))
	}
}

func TestVoiceFromANodeThatIsNotARecipientNeverPlays(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	if _, err := a.app.SendVoice(context.Background(), b.hex(), recording(40)); err != nil {
		t.Fatal(err)
	}
	b.net.err = files.ErrCorrupt // what files.Fetch says when the file does not prove
	id, err := b.app.OnVoice(a.node, voiceTo(t, a, b))
	if err != nil {
		t.Fatal(err)
	}
	until(t, "the fetch", func() bool { c, in, _ := b.net.stats(); return c > 0 && in == 0 })
	time.Sleep(20 * time.Millisecond)
	if _, ok := b.app.VoiceAudio(id); ok {
		t.Fatal("a clip that did not arrive is playable")
	}
}

func FuzzOnFileOffer(f *testing.F) {
	f.Add("e30", "", "00")
	f.Add("", "grp", "")
	good := base64.RawURLEncoding.EncodeToString((files.Offer{ChunkSize: files.ChunkSize}).Marshal())
	f.Add(good, "g", "zz")
	f.Fuzz(func(t *testing.T, meta, group, hash string) {
		b := newWorld().node(t, 12, app.FileLimits{})
		m := &chatpb.PersonalMessageProto{Id: "x", Type: chatpb.PersonalMessageType_MSG_TYPE_FILE, Metadata: meta, ContentHash: hash}
		_ = b.app.OnFileOffer(nid(11), group, m)
		if list, err := b.app.ListFileTransfers(""); err != nil || len(list) > 1000 {
			t.Fatalf("list: %d, %v", len(list), err)
		}
	})
}
