package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/files"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/store"
	"github.com/echofog/echofog-core/internal/v2/voice"
)

var (
	// ErrFilesOff reports a file call on an app built without files/2.
	ErrFilesOff = errors.New("app: files are not available")
	// ErrNoTransfer reports an unknown transfer.
	ErrNoTransfer = errors.New("app: no such transfer")
	// ErrBadFile reports a path that is not a regular file this node can read.
	ErrBadFile = errors.New("app: bad file")
	// ErrFileTooLarge reports a file over the size limit.
	ErrFileTooLarge = errors.New("app: file is too large")
	// ErrTooManyTransfers reports a transfer beyond the number that may run or wait at once.
	ErrTooManyTransfers = errors.New("app: too many transfers")
	// ErrBadState reports a call that does not fit the state of the transfer.
	ErrBadState = errors.New("app: wrong transfer state")
	// ErrBadOffer reports an offer this node refuses; the sender is told nothing.
	ErrBadOffer = errors.New("app: offer refused")
)

// Transfer states and directions, the words of the v1 file screen.
const (
	FileOffered   = "offered"
	FileActive    = "active"
	FileDone      = "done"
	FileDeclined  = "declined"
	FileCancelled = "cancelled"
	FileFailed    = "failed"

	DirIn  = "in"
	DirOut = "out"
)

// Transfer is a file sent to or received from a contact.
type Transfer struct {
	ID    string
	Peer  string // hex node ID of the other side; for a file posted in a group, empty
	Dir   string
	Name  string
	Size  int64
	Mime  string
	State string
	Error string
	// Path is the source of an outgoing file, or the received file once done.
	Path string
	// Done is the bytes received and checked; for an outgoing file Size once every recipient has it.
	Done    int64
	Rate    int64
	Stalled bool
	Group   string
	// Channel and Thread place a file posted in a group, as its message says.
	Channel, Thread string
	// Members, Delivered: for a file posted in a group, who it was offered to and who has all of it.
	Members, Delivered int
	Voice              bool
	Created, Updated   time.Time
}

// FileHolder is the part of *files.Server the app uses.
type FileHolder interface {
	Add(files.Share) error
	Remove(fid [16]byte)
}

// FileNet fetches a file from the node that offered it.
type FileNet interface {
	Fetch(ctx context.Context, from [32]byte, o files.Offer, st files.Store, opt files.FetchOptions) error
}

// FetchOver is a FileNet that opens a files/2 stream to the sender with open and runs files.Fetch on it.
func FetchOver(open func(ctx context.Context, node [32]byte) (*mux.Stream, error)) FileNet {
	return fetchOver{open}
}

type fetchOver struct {
	open func(ctx context.Context, node [32]byte) (*mux.Stream, error)
}

func (f fetchOver) Fetch(ctx context.Context, from [32]byte, o files.Offer, st files.Store, opt files.FetchOptions) error {
	s, err := f.open(ctx, from)
	if err != nil {
		return err
	}
	return files.Fetch(ctx, s, o, st, opt)
}

// FileLimits bound the file domain. Zero fields take the defaults.
type FileLimits struct {
	MaxFileSize  int64         // largest file sent or received
	MaxActive    int           // fetches running at once
	MaxOffers    int           // incoming offers waiting for an answer
	MaxPerSender int           // of them, from one contact
	MaxTransfers int           // records kept (finished ones go first)
	OfferTTL     time.Duration // how long an outgoing offer waits for its recipients
}

// Defaults of FileLimits.
const (
	defaultMaxActive    = 4
	defaultMaxOffers    = 256
	defaultMaxPerSender = 32
	defaultMaxTransfers = 4096
	defaultOfferTTL     = 7 * 24 * time.Hour
	maxVoiceClips       = 64
	fetchRetryFirst     = 15 * time.Second
	fetchRetryMax       = 5 * time.Minute
	transferPrefix      = "ft/"
	maxGroupIDLen       = 64
)

func (l FileLimits) fill() FileLimits {
	if l.MaxFileSize <= 0 || l.MaxFileSize > files.MaxOfferSize {
		l.MaxFileSize = files.DefaultMaxFileSize
	}
	if l.MaxActive <= 0 {
		l.MaxActive = defaultMaxActive
	}
	if l.MaxOffers <= 0 {
		l.MaxOffers = defaultMaxOffers
	}
	if l.MaxPerSender <= 0 {
		l.MaxPerSender = defaultMaxPerSender
	}
	if l.MaxTransfers <= 0 {
		l.MaxTransfers = defaultMaxTransfers
	}
	if l.OfferTTL <= 0 {
		l.OfferTTL = defaultOfferTTL
	}
	return l
}

// xferRec is what survives a restart.
type xferRec struct {
	ID        string   `json:"i"`
	Peer      string   `json:"p,omitempty"`
	Dir       string   `json:"d"`
	Name      string   `json:"n"`
	Mime      string   `json:"m,omitempty"`
	State     string   `json:"s"`
	Error     string   `json:"e,omitempty"`
	Path      string   `json:"f,omitempty"`
	Group     string   `json:"g,omitempty"`
	Size      int64    `json:"z"`
	Done      int64    `json:"o"`
	Voice     bool     `json:"v,omitempty"`
	Created   int64    `json:"c"`
	Updated   int64    `json:"u"`
	Offer     []byte   `json:"r"`
	Members   []string `json:"w,omitempty"`
	Delivered []string `json:"x,omitempty"`
}

type xfer struct {
	rec       xferRec
	offer     files.Offer
	members   map[[32]byte]struct{}
	delivered map[[32]byte]struct{}
	src       io.Closer // the open source of an outgoing file
	entry     *voice.Entry

	running  bool
	stalled  bool
	cancel   context.CancelFunc
	finished chan struct{}
	rate     int64
	lastAt   time.Time
	lastDone int64
}

type voiceRef struct {
	sender [32]byte
	entry  *voice.Entry
}

type fileState struct {
	mu     sync.Mutex
	loaded bool
	m      map[string]*xfer
	subs   map[int]func(Transfer)
	nsub   int
	root   context.Context
	stop   context.CancelFunc
	wg     sync.WaitGroup

	inbox   *voice.Inbox
	voiceBy map[string]voiceRef
	voiceQ  []string
}

func final(state string) bool {
	switch state {
	case FileDone, FileDeclined, FileCancelled, FileFailed:
		return true
	}
	return false
}

func (a *App) limits() FileLimits { return a.cfg.FileLimits.fill() }

func (a *App) now() time.Time { return a.cfg.Clock.Now() }

func nodeOfHex(s string) ([32]byte, bool) {
	var n [32]byte
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return n, false
	}
	copy(n[:], raw)
	return n, true
}

// load reads the saved records once; the caller holds fx.mu. A file this process was sending, or a
// voice clip it was fetching, cannot go on after a restart: the source or the message is gone.
func (a *App) load() {
	s := &a.fx
	if s.loaded {
		return
	}
	s.loaded = true
	s.m = map[string]*xfer{}
	s.subs = map[int]func(Transfer){}
	s.voiceBy = map[string]voiceRef{}
	s.inbox = voice.NewInbox(maxVoiceClips)
	var start []byte
	for len(s.m) < 2*a.limits().MaxTransfers {
		page, err := a.cfg.Store.ScanFrom([]byte(transferPrefix), start, store.MaxScanLimit)
		if err != nil || len(page) == 0 {
			break
		}
		for _, e := range page {
			start = e.Key
			var r xferRec
			if json.Unmarshal(e.Value, &r) != nil {
				continue
			}
			o, err := files.ParseOffer(r.Offer)
			if err != nil || o.Validate() != nil {
				continue
			}
			x := &xfer{rec: r, offer: o}
			restoreSets(x)
			if !final(r.State) && (r.Dir == DirOut || r.Voice) {
				x.rec.State, x.rec.Error = FileFailed, "interrupted by a restart"
				a.save(x)
			}
			s.m[r.ID] = x
		}
	}
}

func restoreSets(x *xfer) {
	x.members, x.delivered = map[[32]byte]struct{}{}, map[[32]byte]struct{}{}
	for _, h := range x.rec.Members {
		if n, ok := nodeOfHex(h); ok {
			x.members[n] = struct{}{}
		}
	}
	for _, h := range x.rec.Delivered {
		if n, ok := nodeOfHex(h); ok {
			x.delivered[n] = struct{}{}
		}
	}
}

func (a *App) save(x *xfer) {
	x.rec.Updated = a.now().UnixMilli()
	if v, err := json.Marshal(x.rec); err == nil {
		_ = a.cfg.Store.Put([]byte(transferPrefix+x.rec.ID), v)
	}
}

func snap(x *xfer) Transfer {
	r := x.rec
	t := Transfer{ID: r.ID, Peer: r.Peer, Dir: r.Dir, Name: r.Name, Size: r.Size, Mime: r.Mime, State: r.State,
		Error: r.Error, Group: r.Group, Done: r.Done, Voice: r.Voice,
		Created: time.UnixMilli(r.Created), Updated: time.UnixMilli(r.Updated)}
	switch r.Dir {
	case DirIn:
		if r.State == FileDone {
			t.Path = r.Path
		}
		t.Stalled = r.State == FileActive && (!x.running || x.stalled)
		if x.running && !x.stalled {
			t.Rate = x.rate
		}
	case DirOut:
		t.Path = r.Path
		if r.Group != "" {
			t.Members, t.Delivered = len(x.members), len(x.delivered)
		}
	}
	return t
}

// emit tells the subscribers; the caller does not hold the lock.
func (a *App) emit(ts ...Transfer) {
	s := &a.fx
	s.mu.Lock()
	a.load()
	keys := make([]int, 0, len(s.subs))
	for k := range s.subs {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	cbs := make([]func(Transfer), 0, len(keys))
	for _, k := range keys {
		cbs = append(cbs, s.subs[k])
	}
	s.mu.Unlock()
	for _, t := range ts {
		for _, cb := range cbs {
			cb(t)
		}
	}
}

// OnFileTransfer calls cb whenever a transfer changes; the returned function stops it. cb must not
// call back into the app.
func (a *App) OnFileTransfer(cb func(Transfer)) func() {
	s := &a.fx
	s.mu.Lock()
	defer s.mu.Unlock()
	a.load()
	s.nsub++
	id := s.nsub
	s.subs[id] = cb
	return func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

// ctx is the context of the running fetches; the caller holds fx.mu.
func (a *App) ctx() context.Context {
	s := &a.fx
	if s.root == nil || s.root.Err() != nil {
		s.root, s.stop = context.WithCancel(context.Background())
	}
	return s.root
}

// stopFiles ends the running fetches; what they verified stays on disk for the next start.
func (a *App) stopFiles() {
	s := &a.fx
	s.mu.Lock()
	if s.stop != nil {
		s.stop()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// get finds a transfer; the caller holds fx.mu.
func (a *App) get(id string) (*xfer, error) {
	a.load()
	x, ok := a.fx.m[strings.ToLower(id)]
	if !ok {
		return nil, ErrNoTransfer
	}
	return x, nil
}

// sweepLocked ends the outgoing offers nobody took in time. It returns what changed and the shares to
// drop; the caller does not hold the lock when it uses them.
func (a *App) sweepLocked() (changed []Transfer, drop []*xfer) {
	ttl, now := a.limits().OfferTTL, a.now()
	for _, x := range a.fx.m {
		if x.rec.Dir == DirOut && x.rec.State == FileOffered && now.Sub(time.UnixMilli(x.rec.Created)) > ttl {
			x.rec.State, x.rec.Error = FileFailed, "expired: nobody took the file"
			a.save(x)
			changed = append(changed, snap(x))
			drop = append(drop, x)
		}
	}
	return
}

// release stops sharing and closes the source of outgoing transfers.
func (a *App) release(drop ...*xfer) {
	for _, x := range drop {
		if a.cfg.Files != nil {
			a.cfg.Files.Remove(x.offer.FID)
		}
		a.fx.mu.Lock()
		src := x.src
		x.src = nil
		a.fx.mu.Unlock()
		if src != nil {
			_ = src.Close()
		}
	}
}

func (a *App) sweep() {
	a.fx.mu.Lock()
	a.load()
	changed, drop := a.sweepLocked()
	a.fx.mu.Unlock()
	a.release(drop...)
	a.emit(changed...)
}

// roomLocked makes space for one more record: the oldest finished ones go first.
func (a *App) roomLocked() error {
	limit := a.limits().MaxTransfers
	for len(a.fx.m) >= limit {
		var old *xfer
		for _, x := range a.fx.m {
			if final(x.rec.State) && (old == nil || x.rec.Updated < old.rec.Updated) {
				old = x
			}
		}
		if old == nil {
			return ErrTooManyTransfers
		}
		delete(a.fx.m, old.rec.ID)
		_ = a.cfg.Store.Delete([]byte(transferPrefix + old.rec.ID))
	}
	return nil
}

// ---- sending ----------------------------------------------------------------------------------------------------

// contactOK checks that peer is a contact and not blocked.
func (a *App) contactOK(peer [32]byte) error {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	r, err := a.loadContact(hex.EncodeToString(peer[:]))
	if err != nil {
		return err
	}
	if r.Blocked {
		return ErrBlocked
	}
	return nil
}

// publish records the transfer, registers the share and sends msg to every recipient. Nothing stays
// behind when a step fails.
func (a *App) publish(ctx context.Context, sh files.Share, src io.Closer, path, peerHex, group string, clip bool, msg *chatpb.PersonalMessageProto) (Transfer, error) {
	o := sh.Offer
	x := &xfer{offer: o, src: src, members: map[[32]byte]struct{}{}, delivered: map[[32]byte]struct{}{},
		rec: xferRec{ID: hex.EncodeToString(o.FID[:]), Peer: peerHex, Dir: DirOut, Name: o.Name, Mime: o.Mime, State: FileOffered,
			Path: path, Group: group, Size: int64(o.Size), Voice: clip, Created: a.now().UnixMilli(), Offer: o.Marshal()}}
	for _, r := range sh.Recipients {
		x.members[r] = struct{}{}
		x.rec.Members = append(x.rec.Members, hex.EncodeToString(r[:]))
	}
	fail := func(err error) (Transfer, error) {
		a.fx.mu.Lock()
		delete(a.fx.m, x.rec.ID)
		_ = a.cfg.Store.Delete([]byte(transferPrefix + x.rec.ID))
		a.fx.mu.Unlock()
		a.release(x)
		return Transfer{}, err
	}
	a.fx.mu.Lock()
	a.load()
	if err := a.roomLocked(); err != nil {
		a.fx.mu.Unlock()
		if src != nil {
			_ = src.Close()
		}
		return Transfer{}, err
	}
	a.fx.m[x.rec.ID] = x
	a.save(x)
	a.fx.mu.Unlock()
	if err := a.cfg.Files.Add(sh); err != nil {
		return fail(err)
	}
	for _, to := range sh.Recipients {
		if _, err := a.cfg.Chat.Send(ctx, to, msg); err != nil {
			return fail(err)
		}
	}
	a.fx.mu.Lock()
	t := snap(x)
	a.fx.mu.Unlock()
	a.emit(t)
	return t, nil
}

func (a *App) prepare(path string, recipients [][32]byte) (files.Share, io.Closer, error) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return files.Share{}, nil, ErrBadFile
	}
	if st.Size() > a.limits().MaxFileSize {
		return files.Share{}, nil, ErrFileTooLarge
	}
	f, err := os.Open(path)
	if err != nil {
		return files.Share{}, nil, ErrBadFile
	}
	sh, err := files.PrepareShare(f, st.Size(), filepath.Base(path), "", a.cfg.Self, recipients)
	if err != nil {
		_ = f.Close()
		return files.Share{}, nil, fmt.Errorf("%w: %v", ErrBadFile, err)
	}
	return sh, f, nil
}

func (a *App) contactNode(peerHex string) ([32]byte, error) {
	peer, ok := nodeOfHex(peerHex)
	if !ok {
		return peer, ErrNoContact
	}
	return peer, a.contactOK(peer)
}

// SendFile offers the file at path to a contact. The transfer stays "offered" until the contact has
// the whole file and says so (FileServed): handing the offer to the chat is not a delivery.
func (a *App) SendFile(ctx context.Context, peerHex, path string) (Transfer, error) {
	if a.cfg.Files == nil || a.cfg.Chat == nil {
		return Transfer{}, ErrFilesOff
	}
	peer, err := a.contactNode(peerHex)
	if err != nil {
		return Transfer{}, err
	}
	a.sweep()
	sh, src, err := a.prepare(path, [][32]byte{peer})
	if err != nil {
		return Transfer{}, err
	}
	return a.publish(ctx, sh, src, path, strings.ToLower(peerHex), "", false, sh.Offer.Message())
}

// SendGroupFile posts the file at path in a group, to the channel and the thread the sender chose.
func (a *App) SendGroupFile(ctx context.Context, group, channel, thread, path string) (Transfer, error) {
	return Transfer{}, errors.New("not built")
}

// FileServed records that a recipient confirmed the whole file (files.ServerConfig.OnDone). Only a
// recipient of the offer counts, and once.
func (a *App) FileServed(fid [16]byte, node [32]byte) {
	a.sweep()
	s := &a.fx
	s.mu.Lock()
	x, ok := s.m[hex.EncodeToString(fid[:])]
	if !ok || x.rec.Dir != DirOut || x.rec.State != FileOffered {
		s.mu.Unlock()
		return
	}
	if _, member := x.members[node]; !member {
		s.mu.Unlock()
		return
	}
	if _, dup := x.delivered[node]; dup {
		s.mu.Unlock()
		return
	}
	x.delivered[node] = struct{}{}
	x.rec.Delivered = append(x.rec.Delivered, hex.EncodeToString(node[:]))
	complete := len(x.delivered) == len(x.members)
	if complete {
		x.rec.State, x.rec.Done = FileDone, x.rec.Size
	}
	a.save(x)
	t := snap(x)
	s.mu.Unlock()
	if complete {
		a.release(x)
	}
	a.emit(t)
}

// ---- receiving --------------------------------------------------------------------------------------------------

// OnFileOffer takes a FILE message the chat layer delivered; from is the sender it verified. group is
// "" for a direct message (the sender must be a contact) or the group the message came in, where the
// caller has checked that the sender is a member. Every refusal is the same ErrBadOffer and nothing
// is sent back.
func (a *App) OnFileOffer(from [32]byte, m *chatpb.PersonalMessageProto) error {
	group := a.groupOfSender(context.Background(), from)
	if a.cfg.FileNet == nil {
		return ErrFilesOff
	}
	if m == nil {
		return ErrBadOffer
	}
	o, err := files.OfferFrom(m)
	if err != nil {
		return ErrBadOffer
	}
	_, err = a.addIncoming(from, group, o, nil)
	return err
}

func (a *App) addIncoming(from [32]byte, group string, o files.Offer, e *voice.Entry) (*xfer, error) {
	lim := a.limits()
	if o.Sender != from || len(group) > maxGroupIDLen || int64(o.Size) > lim.MaxFileSize || int64(o.Size) > files.MaxOfferSize {
		return nil, ErrBadOffer
	}
	if err := a.contactOK(from); err != nil {
		// a group member need not be a contact; a blocked contact is refused everywhere
		if group == "" || errors.Is(err, ErrBlocked) {
			return nil, ErrBadOffer
		}
	}
	id := hex.EncodeToString(o.FID[:])
	peer := hex.EncodeToString(from[:])
	s := &a.fx
	s.mu.Lock()
	a.load()
	if old, ok := s.m[id]; ok {
		s.mu.Unlock()
		if old.rec.Dir == DirIn && old.offer.Sender == from && old.offer.Root == o.Root {
			return old, nil
		}
		return nil, ErrBadOffer
	}
	pending, mine := 0, 0
	for _, x := range s.m {
		if x.rec.Dir == DirIn && x.rec.State == FileOffered {
			pending++
			if x.rec.Peer == peer {
				mine++
			}
		}
	}
	if pending >= lim.MaxOffers || mine >= lim.MaxPerSender {
		s.mu.Unlock()
		return nil, ErrTooManyTransfers
	}
	if err := a.roomLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	x := &xfer{offer: o, entry: e, members: map[[32]byte]struct{}{}, delivered: map[[32]byte]struct{}{},
		rec: xferRec{ID: id, Peer: peer, Dir: DirIn, Name: o.Name, Mime: o.Mime, State: FileOffered, Group: group,
			Size: int64(o.Size), Voice: e != nil, Created: a.now().UnixMilli(), Offer: o.Marshal()}}
	s.m[id] = x
	a.save(x)
	t := snap(x)
	s.mu.Unlock()
	a.emit(t)
	return x, nil
}

func doneBytes(chunks int, o files.Offer) int64 {
	if chunks >= o.Chunks() {
		return int64(o.Size)
	}
	return min(int64(chunks)*int64(files.ChunkSize), int64(o.Size))
}

// AcceptFile starts (or, after a restart or a stall, resumes) the download of an offered file. The
// chunks already verified are kept: the store is bound to the root of the offer.
func (a *App) AcceptFile(id string) (Transfer, error) {
	if a.cfg.FileNet == nil {
		return Transfer{}, ErrFilesOff
	}
	s := &a.fx
	s.mu.Lock()
	x, err := a.get(id)
	if err != nil {
		s.mu.Unlock()
		return Transfer{}, err
	}
	if x.rec.Dir != DirIn || !(x.rec.State == FileOffered || (x.rec.State == FileActive && !x.running)) {
		s.mu.Unlock()
		return Transfer{}, ErrBadState
	}
	running := 0
	for _, y := range s.m {
		if y.running {
			running++
		}
	}
	if running >= a.limits().MaxActive {
		s.mu.Unlock()
		return Transfer{}, ErrTooManyTransfers
	}
	// hold the slot while the store is opened
	x.running = true
	x.finished = make(chan struct{})
	offer := x.offer
	s.mu.Unlock()

	st, err := files.OpenFileStore(a.cfg.FileDir, offer, a.cfg.FileFree)
	s.mu.Lock()
	if err != nil || final(x.rec.State) {
		x.running = false
		close(x.finished)
		s.mu.Unlock()
		if err == nil {
			_ = st.Close()
			if x.rec.State == FileCancelled {
				a.dropPartial(offer)
			}
			err = ErrBadState
		}
		return Transfer{}, err
	}
	ctx, cancel := context.WithCancel(a.ctx())
	x.cancel = cancel
	x.stalled = false
	x.rec.State, x.rec.Error = FileActive, ""
	x.rec.Done = doneBytes(st.Count(), offer)
	x.rate, x.lastAt, x.lastDone = 0, a.now(), x.rec.Done
	a.save(x)
	t := snap(x)
	s.wg.Add(1)
	s.mu.Unlock()
	a.emit(t)
	go a.run(ctx, x, st)
	return t, nil
}

// run fetches the file; a sender that is not there is waited for, any other failure ends the transfer.
func (a *App) run(ctx context.Context, x *xfer, st *files.FileStore) {
	s := &a.fx
	defer s.wg.Done()
	defer func() {
		_ = st.Close()
		s.mu.Lock()
		cancelled := x.rec.State == FileCancelled
		s.mu.Unlock()
		if cancelled {
			a.dropPartial(x.offer)
		}
		s.mu.Lock()
		x.running, x.cancel = false, nil
		close(x.finished)
		s.mu.Unlock()
	}()
	wait := fetchRetryFirst
	for {
		err := a.cfg.FileNet.Fetch(ctx, x.offer.Sender, x.offer, st, files.FetchOptions{
			OnProgress: func(done, _ int) { a.progress(x, doneBytes(done, x.offer)) },
		})
		switch {
		case err == nil:
			a.finish(x, st)
			return
		case ctx.Err() != nil:
			return
		case files.Waiting(err):
			a.stall(x, doneBytes(st.Count(), x.offer), true)
			if !a.waitRetry(ctx, wait) {
				return
			}
			wait = min(wait*2, fetchRetryMax)
			a.stall(x, doneBytes(st.Count(), x.offer), false)
		default:
			a.fail(x, err)
			return
		}
	}
}

func (a *App) waitRetry(ctx context.Context, d time.Duration) bool {
	if a.cfg.FileWait != nil {
		return a.cfg.FileWait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (a *App) progress(x *xfer, done int64) {
	s := &a.fx
	s.mu.Lock()
	if x.rec.State != FileActive {
		s.mu.Unlock()
		return
	}
	now := a.now()
	if dt := now.Sub(x.lastAt); dt > 0 && done >= x.lastDone {
		inst := int64(float64(done-x.lastDone) / dt.Seconds())
		x.rate = (x.rate*7 + inst*3) / 10
		x.lastAt, x.lastDone = now, done
	}
	x.rec.Done = done
	t := snap(x)
	s.mu.Unlock()
	a.emit(t)
}

func (a *App) stall(x *xfer, done int64, stalled bool) {
	s := &a.fx
	s.mu.Lock()
	if x.rec.State != FileActive {
		s.mu.Unlock()
		return
	}
	x.stalled, x.rec.Done = stalled, done
	if stalled {
		a.save(x)
	}
	t := snap(x)
	s.mu.Unlock()
	a.emit(t)
}

func (a *App) fail(x *xfer, err error) {
	s := &a.fx
	s.mu.Lock()
	if final(x.rec.State) {
		s.mu.Unlock()
		return
	}
	x.rec.State, x.rec.Error, x.stalled = FileFailed, err.Error(), false
	a.save(x)
	t := snap(x)
	s.mu.Unlock()
	a.emit(t)
}

// finish ends a download whose store committed. A voice clip is cleaned again and kept in memory;
// the file it came in is deleted.
func (a *App) finish(x *xfer, st *files.FileStore) {
	path := st.Path()
	if x.rec.Voice {
		raw, err := os.ReadFile(path)
		_ = os.Remove(path)
		if err == nil {
			if x.entry == nil {
				err = errors.New("the voice message is gone")
			} else {
				err = x.entry.Finish(raw)
			}
		}
		if err != nil {
			a.fail(x, err)
			return
		}
		path = ""
	}
	s := &a.fx
	s.mu.Lock()
	if final(x.rec.State) {
		s.mu.Unlock()
		return
	}
	x.rec.State, x.rec.Path, x.rec.Done, x.rec.Error, x.stalled = FileDone, path, x.rec.Size, "", false
	a.save(x)
	t := snap(x)
	s.mu.Unlock()
	a.emit(t)
}

// dropPartial removes what a download left on disk.
func (a *App) dropPartial(o files.Offer) {
	id := hex.EncodeToString(o.FID[:])
	for _, ext := range []string{".part", ".bits", ".leaves"} {
		_ = os.Remove(filepath.Join(a.cfg.FileDir, id+ext))
	}
}

// DeclineFile refuses an offered file. The sender is not told: it learns nothing from the refusal.
func (a *App) DeclineFile(id string) error {
	s := &a.fx
	s.mu.Lock()
	x, err := a.get(id)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if x.rec.Dir != DirIn || x.rec.State != FileOffered {
		s.mu.Unlock()
		return ErrBadState
	}
	x.rec.State = FileDeclined
	a.save(x)
	t := snap(x)
	s.mu.Unlock()
	a.emit(t)
	return nil
}

// CancelFile stops a transfer: an outgoing offer stops being shared, a running download ends and its
// partial file goes. A finished transfer cannot be cancelled.
func (a *App) CancelFile(id string) error {
	s := &a.fx
	s.mu.Lock()
	x, err := a.get(id)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if final(x.rec.State) {
		s.mu.Unlock()
		return ErrBadState
	}
	x.rec.State, x.rec.Error, x.stalled = FileCancelled, "", false
	a.save(x)
	t := snap(x)
	cancel, fin, running := x.cancel, x.finished, x.running
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	switch {
	case running:
		<-fin // the download has ended and removed its files
	case t.Dir == DirIn:
		a.dropPartial(x.offer)
	}
	if t.Dir == DirOut {
		a.release(x)
	}
	a.emit(t)
	return nil
}

// ---- reading ----------------------------------------------------------------------------------------------------

// GetFileTransfer returns one transfer.
func (a *App) GetFileTransfer(id string) (Transfer, error) {
	a.sweep()
	s := &a.fx
	s.mu.Lock()
	defer s.mu.Unlock()
	x, err := a.get(id)
	if err != nil {
		return Transfer{}, err
	}
	return snap(x), nil
}

// ListFileTransfers lists the transfers with a contact ("" for all), newest first.
func (a *App) ListFileTransfers(peerHex string) ([]Transfer, error) {
	a.sweep()
	s := &a.fx
	s.mu.Lock()
	defer s.mu.Unlock()
	peerHex = strings.ToLower(peerHex)
	var out []Transfer
	for _, x := range s.m {
		if peerHex == "" || x.rec.Peer == peerHex {
			out = append(out, snap(x))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// FileTransferPath is the file of a finished transfer, or the source of an outgoing one.
func (a *App) FileTransferPath(id string) (string, error) {
	s := &a.fx
	s.mu.Lock()
	defer s.mu.Unlock()
	x, err := a.get(id)
	if err != nil {
		return "", err
	}
	if x.rec.Dir == DirOut || (x.rec.State == FileDone && x.rec.Path != "") {
		return x.rec.Path, nil
	}
	return "", ErrBadState
}

// DownloadDir is where received files go.
func (a *App) DownloadDir() string { return a.cfg.FileDir }

// ---- voice messages ---------------------------------------------------------------------------------------------

// VoiceSent is the result of SendVoice.
type VoiceSent struct {
	ID       string
	Transfer *Transfer // the file of a long clip; nil for a clip inside the message
}

// SendVoice sends a recording to a contact as a voice message: a short clip rides in the message, a
// longer one is a files/2 file.
func (a *App) SendVoice(ctx context.Context, peerHex string, rec voice.Recording) (VoiceSent, error) {
	if a.cfg.Chat == nil {
		return VoiceSent{}, ErrFilesOff
	}
	peer, err := a.contactNode(peerHex)
	if err != nil {
		return VoiceSent{}, err
	}
	d, err := voice.Compose(rec, a.cfg.Self, [][32]byte{peer})
	if err != nil {
		return VoiceSent{}, err
	}
	if d.Share == nil {
		if _, err := a.cfg.Chat.Send(ctx, peer, d.Message); err != nil {
			return VoiceSent{}, err
		}
		return VoiceSent{ID: d.Message.Id}, nil
	}
	if a.cfg.Files == nil {
		return VoiceSent{}, ErrFilesOff
	}
	a.sweep()
	t, err := a.publish(ctx, *d.Share, nil, "", strings.ToLower(peerHex), "", true, d.Message)
	if err != nil {
		return VoiceSent{}, err
	}
	return VoiceSent{ID: d.Message.Id, Transfer: &t}, nil
}

// OnVoice takes a VOICE message the chat layer delivered; from is the sender it verified, who must be
// a contact. A short clip is playable at once; a file clip is fetched without asking, because it is
// the message itself, and never stays in the downloads. It returns the message ID.
func (a *App) OnVoice(from [32]byte, m *chatpb.PersonalMessageProto) (string, error) {
	if m == nil || a.contactOK(from) != nil {
		return "", ErrBadOffer
	}
	s := &a.fx
	s.mu.Lock()
	a.load()
	if ref, taken := s.voiceBy[m.Id]; taken && ref.sender != from {
		s.mu.Unlock()
		return "", ErrBadOffer
	}
	e, _, err := s.inbox.Add(from, m)
	if err != nil {
		s.mu.Unlock()
		return "", ErrBadOffer
	}
	if _, kept := s.voiceBy[m.Id]; !kept {
		s.voiceBy[m.Id] = voiceRef{from, e}
		s.voiceQ = append(s.voiceQ, m.Id)
		for len(s.voiceQ) > maxVoiceClips {
			delete(s.voiceBy, s.voiceQ[0])
			s.voiceQ = s.voiceQ[1:]
		}
	}
	s.mu.Unlock()
	o, isFile := e.Offer()
	if !isFile {
		return m.Id, nil
	}
	if a.cfg.FileNet == nil {
		return "", ErrFilesOff
	}
	x, err := a.addIncoming(from, "", o, e)
	if err != nil {
		return "", err
	}
	if _, err := a.AcceptFile(x.rec.ID); err != nil && !errors.Is(err, ErrBadState) && !errors.Is(err, ErrTooManyTransfers) {
		return "", err
	}
	return m.Id, nil
}

// VoiceAudio is the clean clip of a received voice message, once there is one.
func (a *App) VoiceAudio(id string) ([]byte, bool) {
	s := &a.fx
	s.mu.Lock()
	a.load()
	ref, ok := s.voiceBy[id]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	return ref.entry.Audio()
}
