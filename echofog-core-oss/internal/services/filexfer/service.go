package filexfer

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// DefaultMaxSize bounds what is sent or accepted. The sender keeps the file's whole tree in memory
// while it serves it (64 bytes per 64 KiB chunk, 32 MiB for this size).
const DefaultMaxSize int64 = 32 << 30

// spaceMargin stays free on the disk besides the file being received.
const spaceMargin = 64 << 20

// Transport delivers one frame to a peer: directly when connected, else through a relay circuit.
type Transport interface {
	Send(ctx context.Context, peer types.PeerID, frame []byte) error
}

// Offer is what the receiver learns about a file, inside an end-to-end encrypted message.
type Offer struct {
	ID    string
	Group string // set for a file posted in a group chat: every member gets the same offer
	Name  string
	Size  int64
	Mime  string
	Root  string // hex SHA-256 Merkle root of the chunks
	Key   string // hex 32-byte secret the transfer frames are sealed with
}

// Info is a transfer as the user sees it.
type Info struct {
	ID      string    `json:"id"`
	Peer    string    `json:"peer"`            // the other side (empty for a file we posted in a group)
	Group   string    `json:"group,omitempty"` // the group chat the file was posted in
	Dir     Direction `json:"dir"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mime    string    `json:"mime,omitempty"`
	State   State     `json:"state"`
	Error   string    `json:"error,omitempty"`
	Path    string    `json:"path,omitempty"`
	Done    int64     `json:"done"`    // bytes received and checked (incoming) or sent (outgoing)
	Rate    int64     `json:"rate"`    // bytes per second, lately
	Stalled bool      `json:"stalled"` // nothing moves: the peer is offline or cannot be reached
	// a file we posted in a group: members it was offered to, got all of it, are fetching it now
	Members   int   `json:"members,omitempty"`
	Delivered int   `json:"delivered,omitempty"`
	Fetching  int   `json:"fetching,omitempty"`
	Created   int64 `json:"created"`
	Updated   int64 `json:"updated"`
}

// Options set up a Service.
type Options struct {
	Store     storage.Storage
	Transport Transport
	// Self is this node: a group member names itself in what it sends to the sender.
	Self types.PeerID
	// IsMember, when set, tells whether a peer is (now) a member of a group: besides the members
	// a group offer went to, the sender serves those it counts.
	IsMember func(group string, peer types.PeerID) bool
	// DownloadDir is where accepted files go; it is asked at every accept, so a changed setting applies.
	DownloadDir func() string
	MaxSize     int64
	// Connect, when set, is tried when frames cannot reach the peer (a DHT lookup, a hole punch).
	Connect func(ctx context.Context, peer types.PeerID) error
	// Emit receives a transfer whenever it changes, at most a few times a second while it runs.
	Emit   func(Info)
	Logger *slog.Logger

	// tuning, mostly for tests
	InitialWindow int           // chunks in flight at the start
	MaxWindow     int           // chunks in flight at most
	MinRTO        time.Duration // the shortest wait before a chunk is asked for again
	StallAfter    time.Duration // no chunk for this long: the transfer counts as stalled
	FreeSpace     func(dir string) (uint64, error)
}

// Service runs the transfers of one node.
type Service struct {
	opt    Options
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu  sync.Mutex
	out map[[idLen]byte]*outgoing
	in  map[[idLen]byte]*incoming

	// Dropped counts frames thrown away because they did not open or did not verify (tests watch it).
	dropped sync.Map
}

// New makes a service; Start brings back the transfers of an earlier run.
func New(opt Options) *Service {
	if opt.MaxSize <= 0 {
		opt.MaxSize = DefaultMaxSize
	}
	if opt.InitialWindow <= 0 {
		opt.InitialWindow = 16
	}
	if opt.MaxWindow <= 0 {
		opt.MaxWindow = 256
	}
	if opt.MinRTO <= 0 {
		opt.MinRTO = 1500 * time.Millisecond
	}
	if opt.StallAfter <= 0 {
		opt.StallAfter = 15 * time.Second
	}
	if opt.FreeSpace == nil {
		opt.FreeSpace = FreeSpace
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		opt: opt, log: opt.Logger.With("component", "filexfer"), ctx: ctx, cancel: cancel,
		out: map[[idLen]byte]*outgoing{}, in: map[[idLen]byte]*incoming{},
	}
}

// Start loads the transfers kept from before and resumes the downloads that were under way.
func (s *Service) Start() error {
	recs, err := loadRecords(s.opt.Store)
	if err != nil {
		return err
	}
	for _, r := range recs {
		id, peer, k, root, err := parseRecord(r)
		if err != nil {
			s.log.Warn("FileXfer: dropping unreadable transfer", "id", r.ID, "err", err)
			deleteRecord(s.opt.Store, r.ID)
			continue
		}
		switch r.Dir {
		case Outgoing:
			o := newOutgoing(r, id, peer, k, root)
			s.mu.Lock()
			s.out[id] = o
			s.mu.Unlock()
		case Incoming:
			in := newIncoming(r, id, peer, k, root)
			s.mu.Lock()
			s.in[id] = in
			s.mu.Unlock()
			if r.State == StateActive {
				in.bits = loadBits(s.opt.Store, r.ID, in.n)
				in.recount()
				if err := s.startIncoming(in); err != nil {
					s.failIncoming(in, err)
				}
			}
		}
	}
	return nil
}

// Stop ends all loops; downloads keep their progress and continue at the next Start.
func (s *Service) Stop() {
	s.cancel()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.out {
		o.closeFile()
	}
	for _, in := range s.in {
		in.fmu.Lock()
		if in.f != nil {
			_ = in.f.Close()
			in.f = nil
		}
		in.fmu.Unlock()
		in.mu.Lock()
		in.running = false
		in.mu.Unlock()
	}
}

func parseRecord(r *Record) (id [idLen]byte, peer types.PeerID, k keys, root Hash, err error) {
	raw, err := hex.DecodeString(r.ID)
	if err != nil || len(raw) != idLen {
		return id, peer, k, root, errors.New("bad transfer id")
	}
	copy(id[:], raw)
	if r.Peer != "" || r.Group == "" || r.Dir != Outgoing {
		if peer, err = types.PeerIDFromString(r.Peer); err != nil {
			return id, peer, k, root, err
		}
	}
	rb, err := hex.DecodeString(r.Root)
	if err != nil || len(rb) != len(root) {
		return id, peer, k, root, errors.New("bad root")
	}
	copy(root[:], rb)
	kb, err := hex.DecodeString(r.Key)
	if err != nil {
		return id, peer, k, root, err
	}
	k, err = deriveKeys(kb)
	return id, peer, k, root, err
}

func nowMs() int64 { return time.Now().UnixMilli() }

// ---- sending ---------------------------------------------------------------------------------

// Prepare reads and hashes a file to send to peer and returns the offer to put in a message. The
// file is served from where it is, as long as it does not change.
func (s *Service) Prepare(peer types.PeerID, path string) (Offer, error) {
	return s.prepare(peer, "", nil, path)
}

// PrepareGroup is Prepare for a file posted in a group: one offer (one key) goes to all members,
// and each member who accepts pulls the file with its own progress. Wants count only from members:
// those the offer went to, or anyone Options.IsMember still counts as one.
func (s *Service) PrepareGroup(group string, members []types.PeerID, path string) (Offer, error) {
	if group == "" {
		return Offer{}, errors.New("no group")
	}
	return s.prepare(types.PeerID{}, group, members, path)
}

func (s *Service) prepare(peer types.PeerID, group string, members []types.PeerID, path string) (Offer, error) {
	path = filepath.Clean(path)
	st, err := os.Stat(path)
	if err != nil {
		return Offer{}, err
	}
	if !st.Mode().IsRegular() {
		return Offer{}, errors.New("not a regular file")
	}
	if st.Size() > s.opt.MaxSize {
		return Offer{}, fmt.Errorf("file is larger than %s", humanSize(s.opt.MaxSize))
	}
	f, err := os.Open(path)
	if err != nil {
		return Offer{}, err
	}
	defer f.Close()
	tree, err := HashFile(f, st.Size())
	if err != nil {
		return Offer{}, err
	}

	var id [idLen]byte
	secret := make([]byte, 32)
	_, _ = rand.Read(id[:])
	_, _ = rand.Read(secret)
	k, _ := deriveKeys(secret)
	root := tree.Root()
	name := SafeName(filepath.Base(path))
	r := &Record{
		ID: hex.EncodeToString(id[:]), Group: group, Dir: Outgoing, Name: name, Size: st.Size(),
		Mime: mime.TypeByExtension(filepath.Ext(name)), Root: hex.EncodeToString(root[:]), Key: hex.EncodeToString(secret),
		Path: path, ModTime: st.ModTime().UnixNano(), State: StateOffered, Created: nowMs(), Updated: nowMs(),
	}
	if group == "" {
		r.Peer = peer.String()
	}
	for _, m := range members {
		if m != s.opt.Self && !slices.Contains(r.Members, m.String()) {
			r.Members = append(r.Members, m.String())
		}
	}
	if err := saveRecord(s.opt.Store, r); err != nil {
		return Offer{}, err
	}
	o := newOutgoing(r, id, peer, k, root)
	o.tree = tree
	s.mu.Lock()
	s.out[id] = o
	s.mu.Unlock()
	s.emit(o.info())
	return Offer{ID: r.ID, Group: group, Name: r.Name, Size: r.Size, Mime: r.Mime, Root: r.Root, Key: r.Key}, nil
}

// Withdraw forgets an offer that never reached the peer (its message could not be sent).
func (s *Service) Withdraw(id string) {
	key, ok := idKey(id)
	if !ok {
		return
	}
	s.mu.Lock()
	o := s.out[key]
	delete(s.out, key)
	s.mu.Unlock()
	if o != nil {
		o.stopWorkers()
	}
	deleteRecord(s.opt.Store, id)
}

// outgoing is a file this node offered. A direct transfer has one receiver; a group transfer serves
// every member who asks, each from its own queue and with its own progress, from one open file.
type outgoing struct {
	mu      sync.Mutex
	rec     *Record
	id      [idLen]byte
	peer    types.PeerID // the receiver of a direct transfer; zero for a group
	members map[types.PeerID]bool
	k       keys
	root    Hash
	tree    *Tree
	f       *os.File
	serves  map[types.PeerID]*serving
	workers int // serve loops running; the file is closed when the last one goes idle
}

// serving is the sender's side of one receiver.
type serving struct {
	peer    types.PeerID
	state   State // active until the receiver has all of it (done), declined or cancelled
	queue   chan int
	working bool
	quit    chan struct{}
	sent    []byte // chunks sent at least once, for progress
	nsent   int
	meter   meter
}

func newOutgoing(r *Record, id [idLen]byte, peer types.PeerID, k keys, root Hash) *outgoing {
	o := &outgoing{rec: r, id: id, peer: peer, k: k, root: root, serves: map[types.PeerID]*serving{}, members: map[types.PeerID]bool{}}
	for _, m := range r.Members {
		if p, err := types.PeerIDFromString(m); err == nil {
			o.members[p] = true
		}
	}
	return o
}

func (o *outgoing) group() bool { return o.rec.Group != "" }

// serveFor is the state of one receiver, made on its first want. o.mu is held.
func (o *outgoing) serveFor(peer types.PeerID) *serving {
	sv := o.serves[peer]
	if sv == nil {
		n := NumChunks(o.rec.Size)
		sv = &serving{peer: peer, state: StateActive, queue: make(chan int, 4096), sent: make([]byte, (n+7)/8)}
		o.serves[peer] = sv
	}
	return sv
}

func (o *outgoing) info() Info {
	o.mu.Lock()
	defer o.mu.Unlock()
	var done, rate int64
	fetching := 0
	for p, sv := range o.serves {
		d := int64(sv.nsent) * ChunkSize
		rate += sv.meter.rate()
		switch {
		case !o.group() && p == o.peer:
			done = d
		case sv.state == StateActive:
			fetching++
			done = max(done, d) // the furthest member along
		case sv.state == StateDone:
			done = max(done, d)
		}
	}
	if o.rec.State == StateDone || done > o.rec.Size {
		done = o.rec.Size
	}
	return Info{ID: o.rec.ID, Peer: o.rec.Peer, Group: o.rec.Group, Dir: Outgoing, Name: o.rec.Name, Size: o.rec.Size, Mime: o.rec.Mime,
		State: o.rec.State, Error: o.rec.Error, Path: o.rec.Path, Done: done, Rate: rate,
		Members: len(o.rec.Members), Delivered: len(o.rec.Delivered), Fetching: fetching,
		Created: o.rec.Created, Updated: o.rec.Updated}
}

func (o *outgoing) closeFile() {
	o.mu.Lock()
	if o.f != nil {
		_ = o.f.Close()
		o.f = nil
	}
	o.mu.Unlock()
}

func (o *outgoing) stopWorkers() {
	o.mu.Lock()
	for _, sv := range o.serves {
		sv.stop()
	}
	o.mu.Unlock()
}

// stop ends the serve loop of one receiver; o.mu is held.
func (sv *serving) stop() {
	if sv.quit != nil {
		close(sv.quit)
		sv.quit = nil
	}
}

// receivers is who should hear that the offer is gone: the receiver of a direct transfer, or every
// member of the group (those who never asked would otherwise wait on a file that is not coming).
func (o *outgoing) receivers() []types.PeerID {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.group() {
		return []types.PeerID{o.peer}
	}
	seen := map[types.PeerID]bool{}
	var out []types.PeerID
	for p := range o.members {
		seen[p] = true
		out = append(out, p)
	}
	for p, sv := range o.serves {
		if !seen[p] && sv.state == StateActive {
			out = append(out, p)
		}
	}
	return out
}

// cancelReceivers tells every receiver that the file is no longer served.
func (s *Service) cancelReceivers(o *outgoing, reason byte) {
	var wg sync.WaitGroup
	for _, p := range o.receivers() {
		wg.Add(1)
		go func(p types.PeerID) {
			defer wg.Done()
			s.sendCancel(p, o.k.toReceiver, o.id, reason)
		}(p)
	}
	wg.Wait()
}

func (s *Service) setOutState(o *outgoing, st State, msg string) {
	o.mu.Lock()
	if o.rec.State.Final() && st != o.rec.State {
		o.mu.Unlock()
		return
	}
	o.rec.State, o.rec.Error, o.rec.Updated = st, msg, nowMs()
	_ = saveRecord(s.opt.Store, o.rec)
	o.mu.Unlock()
	if st.Final() {
		o.stopWorkers()
		o.closeFile()
	}
	s.emit(o.info())
}

// receiverOf tells who sent a frame to us as the sender. In a group every member holds the same
// key, so each says who it is inside the sealed body; a direct transfer has only the one receiver.
func (s *Service) receiverOf(o *outgoing, body []byte) (types.PeerID, []byte, bool) {
	if !o.group() {
		return o.peer, body, true
	}
	var p types.PeerID
	if len(body) < len(p) {
		return p, nil, false
	}
	copy(p[:], body)
	o.mu.Lock()
	ok := o.members[p]
	o.mu.Unlock()
	if !ok && s.opt.IsMember != nil {
		ok = s.opt.IsMember(o.rec.Group, p)
	}
	return p, body[len(p):], ok && p != s.opt.Self
}

func (s *Service) handleWant(o *outgoing, frame []byte) {
	body, err := open(o.k.toSender, frame)
	if err != nil {
		s.drop("open")
		return
	}
	peer, body, ok := s.receiverOf(o, body)
	if !ok {
		s.drop("member")
		return
	}
	idx, err := decodeWant(body)
	if err != nil {
		s.drop("want")
		return
	}
	o.mu.Lock()
	state := o.rec.State
	o.mu.Unlock()
	switch {
	case state == StateCancelled:
		s.sendCancel(peer, o.k.toReceiver, o.id, reasonCancelled)
		return
	case state == StateFailed:
		s.sendCancel(peer, o.k.toReceiver, o.id, reasonUnavailable)
		return
	case state.Final():
		return
	case state == StateOffered:
		s.setOutState(o, StateActive, "")
	}
	o.mu.Lock()
	sv := o.serveFor(peer)
	ended := sv.state.Final()
	o.mu.Unlock()
	if ended {
		return
	}
	for _, i := range idx {
		select {
		case sv.queue <- i:
		default: // full: the receiver asks again
		}
	}
	s.startWorker(o, sv)
}

func (s *Service) startWorker(o *outgoing, sv *serving) {
	o.mu.Lock()
	if sv.working {
		o.mu.Unlock()
		return
	}
	sv.working = true
	sv.quit = make(chan struct{})
	quit := sv.quit
	o.workers++
	o.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		idle := s.serve(o, sv, quit)
		o.mu.Lock()
		sv.working = false
		o.workers--
		if idle && o.workers == 0 && o.f != nil {
			_ = o.f.Close() // reopened (and checked) when a receiver asks again
			o.f = nil
		}
		o.mu.Unlock()
	}()
}

// prepareSource opens the file and makes sure it is still the one offered.
func (s *Service) prepareSource(o *outgoing) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.f != nil && o.tree != nil {
		return nil
	}
	st, err := os.Stat(o.rec.Path)
	if err != nil || st.Size() != o.rec.Size {
		return errors.New("the file is gone or changed")
	}
	f, err := os.Open(o.rec.Path)
	if err != nil {
		return err
	}
	if o.tree == nil || st.ModTime().UnixNano() != o.rec.ModTime {
		tree, err := HashFile(f, o.rec.Size)
		if err != nil || tree.Root() != o.root {
			f.Close()
			return errors.New("the file changed since it was offered")
		}
		o.tree = tree
	}
	o.f = f
	return nil
}

// sendPacer limits how many chunk frames are on their way at once. Frames sent one after another
// leave a fast link idle between them; too many at once on a slow link each miss their write
// deadline. So the number grows while frames leave quickly and halves when one is slow or fails.
type sendPacer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	active int
	limit  int
}

const maxParallelSends = 8

func newSendPacer() *sendPacer {
	p := &sendPacer{limit: 2}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *sendPacer) acquire() {
	p.mu.Lock()
	for p.active >= p.limit {
		p.cond.Wait()
	}
	p.active++
	p.mu.Unlock()
}

func (p *sendPacer) release(took time.Duration, failed bool) {
	p.mu.Lock()
	p.active--
	switch {
	case failed || took > 600*time.Millisecond:
		p.limit = max(1, p.limit/2)
	case took < 150*time.Millisecond && p.limit < maxParallelSends:
		p.limit++
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

// serve answers one receiver's wants in the order they came, a few frames at a time (see
// sendPacer). It reports whether it stopped because that receiver went quiet.
func (s *Service) serve(o *outgoing, sv *serving, quit chan struct{}) (idle bool) {
	if err := s.prepareSource(o); err != nil {
		s.log.Warn("FileXfer: cannot serve file", "id", o.rec.ID, "err", err)
		s.setOutState(o, StateFailed, err.Error())
		go s.cancelReceivers(o, reasonUnavailable)
		return false
	}
	pacer := newSendPacer()
	var sends sync.WaitGroup
	defer sends.Wait()
	var broken atomic.Bool
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for !broken.Load() {
		var i int
		select {
		case i = <-sv.queue:
		case <-quit:
			return false
		case <-s.ctx.Done():
			return false
		case <-timer.C:
			sends.Wait()
			return true
		}
		timer.Reset(30 * time.Second)
		if i < 0 || i >= o.tree.Leaves() {
			continue
		}
		o.mu.Lock()
		f := o.f
		o.mu.Unlock()
		if f == nil {
			return false
		}
		pacer.acquire()
		sends.Add(1)
		go func(i int) {
			defer sends.Done()
			l := chunkLen(o.rec.Size, i)
			buf := make([]byte, l)
			if err := readChunk(f, buf, int64(i)*ChunkSize); err != nil {
				pacer.release(0, false)
				o.mu.Lock()
				ended := o.rec.State.Final() // cancelled meanwhile: the file was closed under this read
				o.mu.Unlock()
				if !ended && broken.CompareAndSwap(false, true) {
					s.setOutState(o, StateFailed, "cannot read the file: "+err.Error())
					go s.cancelReceivers(o, reasonUnavailable)
				}
				return
			}
			frame := seal(o.k.toReceiver, kindChunk, o.id, encodeChunk(i, o.tree.Proof(i), buf))
			ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
			start := time.Now()
			err := s.opt.Transport.Send(ctx, sv.peer, frame)
			cancel()
			pacer.release(time.Since(start), err != nil)
			if err != nil {
				return // lost: the receiver asks for it again
			}
			o.mu.Lock()
			if sv.sent[i/8]&(1<<(i%8)) == 0 {
				sv.sent[i/8] |= 1 << (i % 8)
				sv.nsent++
			}
			sv.meter.add(int64(l))
			emit := sv.meter.due()
			o.mu.Unlock()
			if emit {
				s.emit(o.info())
			}
		}(i)
	}
	return false
}

func (s *Service) handleFromReceiver(o *outgoing, kind frameKind, frame []byte) {
	body, err := open(o.k.toSender, frame)
	if err != nil {
		s.drop("open")
		return
	}
	peer, body, ok := s.receiverOf(o, body)
	if !ok {
		s.drop("member")
		return
	}
	declined := len(body) == 1 && body[0] == reasonDeclined
	if !o.group() {
		switch {
		case kind == kindDone:
			s.setOutState(o, StateDone, "")
		case declined:
			s.setOutState(o, StateDeclined, "")
		default:
			s.setOutState(o, StateCancelled, "")
		}
		return
	}
	// in a group, one member is done with the file or does not want it: the others go on
	o.mu.Lock()
	sv := o.serveFor(peer)
	switch {
	case sv.state == StateDone && kind != kindDone:
		// a late cancel after the file arrived changes nothing
	case kind == kindDone:
		sv.state = StateDone
		if !slices.Contains(o.rec.Delivered, peer.String()) {
			o.rec.Delivered = append(o.rec.Delivered, peer.String())
			o.rec.Updated = nowMs()
			_ = saveRecord(s.opt.Store, o.rec)
		}
	case declined:
		sv.state = StateDeclined
	default:
		sv.state = StateCancelled
	}
	sv.stop()
	o.mu.Unlock()
	s.emit(o.info())
}

// ---- receiving -------------------------------------------------------------------------------

// OnOffer records a file a peer offers; nothing is fetched before the user accepts it. A repeated
// offer (the message arrived twice) changes nothing.
func (s *Service) OnOffer(from types.PeerID, o Offer) (Info, error) {
	id, ok := idKey(o.ID)
	if !ok {
		return Info{}, errors.New("bad transfer id")
	}
	if o.Size < 0 {
		return Info{}, errors.New("bad size")
	}
	s.mu.Lock()
	if in := s.in[id]; in != nil {
		s.mu.Unlock()
		return in.info(), nil
	}
	s.mu.Unlock()
	r := &Record{
		ID: o.ID, Peer: from.String(), Group: o.Group, Dir: Incoming, Name: SafeName(o.Name), Size: o.Size, Mime: o.Mime,
		Root: o.Root, Key: o.Key, State: StateOffered, Created: nowMs(), Updated: nowMs(),
	}
	_, peer, k, root, err := parseRecord(r)
	if err != nil {
		return Info{}, err
	}
	if err := saveRecord(s.opt.Store, r); err != nil {
		return Info{}, err
	}
	in := newIncoming(r, id, peer, k, root)
	s.mu.Lock()
	if cur := s.in[id]; cur != nil {
		in = cur
	} else {
		s.in[id] = in
	}
	s.mu.Unlock()
	inf := in.info()
	s.emit(inf)
	return inf, nil
}

type incoming struct {
	mu      sync.Mutex
	life    sync.Mutex // held while the transfer ends or its file is finished, so the two never overlap
	rec     *Record
	id      [idLen]byte
	peer    types.PeerID
	k       keys
	root    Hash
	n       int
	bits    []byte
	have    int
	dirty   bool
	f       *os.File
	fmu     sync.RWMutex // writes hold it shared, closing exclusively
	arrived chan int
	quit    chan struct{}
	running bool
	stalled bool
	retried bool // the whole file failed its final check once and is being fetched again
	meter   meter
}

func newIncoming(r *Record, id [idLen]byte, peer types.PeerID, k keys, root Hash) *incoming {
	n := NumChunks(r.Size)
	return &incoming{rec: r, id: id, peer: peer, k: k, root: root, n: n, bits: make([]byte, (n+7)/8), arrived: make(chan int, 4096)}
}

func (in *incoming) hasChunk(i int) bool { return in.bits[i/8]&(1<<(i%8)) != 0 }

func (in *incoming) recount() {
	in.have = 0
	for i := 0; i < in.n; i++ {
		if in.hasChunk(i) {
			in.have++
		}
	}
}

func (in *incoming) doneBytes() int64 {
	if in.have >= in.n {
		return in.rec.Size
	}
	d := int64(in.have) * ChunkSize
	if in.hasChunk(in.n - 1) {
		d -= ChunkSize - int64(chunkLen(in.rec.Size, in.n-1))
	}
	return d
}

func (in *incoming) info() Info {
	in.mu.Lock()
	defer in.mu.Unlock()
	return Info{ID: in.rec.ID, Peer: in.rec.Peer, Group: in.rec.Group, Dir: Incoming, Name: in.rec.Name, Size: in.rec.Size, Mime: in.rec.Mime,
		State: in.rec.State, Error: in.rec.Error, Path: in.rec.Path, Done: in.doneBytes(), Rate: in.meter.rate(),
		Stalled: in.stalled && in.rec.State == StateActive, Created: in.rec.Created, Updated: in.rec.Updated}
}

// Accept starts fetching an offered file into the download folder.
func (s *Service) Accept(id string) (Info, error) {
	in, err := s.incomingByID(id)
	if err != nil {
		return Info{}, err
	}
	in.mu.Lock()
	state := in.rec.State
	in.mu.Unlock()
	if state != StateOffered {
		if state == StateActive {
			return in.info(), nil
		}
		return Info{}, fmt.Errorf("the transfer is %s", state)
	}
	if in.rec.Size > s.opt.MaxSize {
		return Info{}, fmt.Errorf("file is larger than %s", humanSize(s.opt.MaxSize))
	}
	dir := s.opt.DownloadDir()
	if dir == "" {
		return Info{}, errors.New("no download folder")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Info{}, err
	}
	if free, err := s.opt.FreeSpace(dir); err == nil && free < uint64(in.rec.Size)+spaceMargin {
		return Info{}, fmt.Errorf("not enough disk space: %s needed, %s free", humanSize(in.rec.Size), humanSize(int64(free)))
	}
	final, err := freeName(dir, in.rec.Name)
	if err != nil {
		return Info{}, err
	}
	in.mu.Lock()
	in.rec.Path = final
	in.rec.Part = final + "." + in.rec.ID[:8] + ".part"
	in.rec.State, in.rec.Updated = StateActive, nowMs()
	err = saveRecord(s.opt.Store, in.rec)
	in.mu.Unlock()
	if err != nil {
		return Info{}, err
	}
	if err := s.startIncoming(in); err != nil {
		s.failIncoming(in, err)
		return Info{}, err
	}
	inf := in.info()
	s.emit(inf)
	return inf, nil
}

// Decline turns an offer down and tells the sender.
func (s *Service) Decline(id string) error {
	in, err := s.incomingByID(id)
	if err != nil {
		return err
	}
	in.mu.Lock()
	if in.rec.State != StateOffered {
		in.mu.Unlock()
		return fmt.Errorf("the transfer is %s", in.rec.State)
	}
	in.rec.State, in.rec.Updated = StateDeclined, nowMs()
	_ = saveRecord(s.opt.Store, in.rec)
	in.mu.Unlock()
	go s.sendUp(in, kindCancel, []byte{reasonDeclined})
	s.emit(in.info())
	return nil
}

// Cancel stops a transfer either way; a partial download is deleted.
func (s *Service) Cancel(id string) error {
	key, ok := idKey(id)
	if !ok {
		return errNotFound
	}
	s.mu.Lock()
	in, o := s.in[key], s.out[key]
	s.mu.Unlock()
	switch {
	case o != nil:
		o.mu.Lock()
		final := o.rec.State.Final()
		o.mu.Unlock()
		if final {
			return nil
		}
		s.setOutState(o, StateCancelled, "")
		go s.cancelReceivers(o, reasonCancelled)
		return nil
	case in != nil:
		in.mu.Lock()
		if in.rec.State.Final() {
			in.mu.Unlock()
			return nil
		}
		in.mu.Unlock()
		s.endIncoming(in, StateCancelled, "")
		go s.sendUp(in, kindCancel, []byte{reasonCancelled})
		return nil
	}
	return errNotFound
}

func (s *Service) incomingByID(id string) (*incoming, error) {
	key, ok := idKey(id)
	if !ok {
		return nil, errNotFound
	}
	s.mu.Lock()
	in := s.in[key]
	s.mu.Unlock()
	if in == nil {
		return nil, errNotFound
	}
	return in, nil
}

// startIncoming opens the partial file and runs the fetch loop.
func (s *Service) startIncoming(in *incoming) error {
	in.mu.Lock()
	if in.running {
		in.mu.Unlock()
		return nil
	}
	part := in.rec.Part
	in.mu.Unlock()
	if part == "" || !inside(filepath.Dir(in.rec.Path), part) {
		return errors.New("no place for the download")
	}
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if st, err := f.Stat(); err == nil && st.Size() > in.rec.Size {
		_ = f.Truncate(in.rec.Size)
	}
	in.fmu.Lock()
	in.f = f
	in.fmu.Unlock()
	in.mu.Lock()
	in.running = true
	in.quit = make(chan struct{})
	quit := in.quit
	in.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.fetch(in, quit)
	}()
	return nil
}

func (s *Service) failIncoming(in *incoming, err error) {
	in.life.Lock()
	defer in.life.Unlock()
	s.failIncomingLocked(in, err)
}

func (s *Service) failIncomingLocked(in *incoming, err error) {
	s.log.Warn("FileXfer: download failed", "id", in.rec.ID, "err", err)
	s.endIncomingLocked(in, StateFailed, err.Error())
}

// endIncoming stops the loop and, unless the file is done, deletes what arrived. A transfer that
// already ended (it finished just as the cancel came) stays as it is.
func (s *Service) endIncoming(in *incoming, st State, msg string) {
	in.life.Lock()
	defer in.life.Unlock()
	s.endIncomingLocked(in, st, msg)
}

func (s *Service) endIncomingLocked(in *incoming, st State, msg string) {
	in.mu.Lock()
	if in.rec.State.Final() {
		in.mu.Unlock()
		return
	}
	if in.quit != nil {
		close(in.quit)
		in.quit = nil
	}
	in.running = false
	in.rec.State, in.rec.Error, in.rec.Updated = st, msg, nowMs()
	part := in.rec.Part
	if st != StateDone {
		in.rec.Path = ""
	}
	_ = saveRecord(s.opt.Store, in.rec)
	in.mu.Unlock()
	in.fmu.Lock()
	if in.f != nil {
		_ = in.f.Close()
		in.f = nil
	}
	in.fmu.Unlock()
	if st != StateDone && part != "" {
		if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("FileXfer: cannot delete the partial file", "id", in.rec.ID, "err", err)
		}
		_ = s.opt.Store.Delete(append(append([]byte(nil), bitsPrefix...), in.rec.ID...))
	}
	s.emit(in.info())
}

func (s *Service) handleChunk(in *incoming, frame []byte) {
	body, err := open(in.k.toReceiver, frame)
	if err != nil {
		s.drop("open")
		return
	}
	i, proof, data, err := decodeChunk(body)
	if err != nil || i < 0 || i >= in.n || len(data) != chunkLen(in.rec.Size, i) {
		s.drop("chunk")
		return
	}
	if !VerifyProof(in.root, i, in.n, LeafHash(data), proof) {
		s.drop("proof")
		return
	}
	in.mu.Lock()
	if in.rec.State != StateActive || in.hasChunk(i) {
		in.mu.Unlock()
		return
	}
	in.mu.Unlock()

	in.fmu.RLock()
	if in.f == nil {
		in.fmu.RUnlock()
		return
	}
	_, werr := in.f.WriteAt(data, int64(i)*ChunkSize)
	in.fmu.RUnlock()
	if werr != nil {
		go s.failIncoming(in, fmt.Errorf("cannot write the file: %w", werr))
		return
	}

	in.mu.Lock()
	if !in.hasChunk(i) {
		in.bits[i/8] |= 1 << (i % 8)
		in.have++
		in.dirty = true
		in.meter.add(int64(len(data)))
	}
	in.mu.Unlock()
	select {
	case in.arrived <- i:
	default:
	}
}

func (s *Service) handleFromSender(in *incoming, frame []byte) {
	body, err := open(in.k.toReceiver, frame)
	if err != nil {
		s.drop("open")
		return
	}
	in.mu.Lock()
	st := in.rec.State
	in.mu.Unlock()
	if st.Final() {
		return
	}
	if len(body) == 1 && body[0] == reasonUnavailable {
		s.endIncoming(in, StateFailed, "the sender no longer has the file")
		return
	}
	s.endIncoming(in, StateCancelled, "")
}

// persist makes what was written durable, then records which chunks it holds: after a crash the
// record never claims a chunk the disk may not have.
func (s *Service) persist(in *incoming) {
	in.mu.Lock()
	if !in.dirty || in.rec.State != StateActive {
		in.mu.Unlock()
		return
	}
	in.dirty = false
	in.mu.Unlock()
	in.fmu.RLock()
	if in.f != nil {
		_ = in.f.Sync()
	}
	in.fmu.RUnlock()
	in.mu.Lock()
	bits := append([]byte(nil), in.bits...)
	in.mu.Unlock()
	_ = saveBits(s.opt.Store, in.rec.ID, bits)
}

// finish checks the whole file against the root and moves it to its name.
func (s *Service) finish(in *incoming) bool {
	s.persist(in)
	in.life.Lock()
	defer in.life.Unlock()
	in.mu.Lock()
	ended := in.rec.State != StateActive
	in.mu.Unlock()
	if ended {
		return true
	}
	in.fmu.Lock()
	f := in.f
	in.fmu.Unlock()
	if f == nil {
		return true
	}
	tree, err := HashFile(f, in.rec.Size)
	if err != nil || tree.Root() != in.root {
		in.mu.Lock()
		again := !in.retried
		in.retried = true
		if again {
			// a chunk on disk is not what was written (the disk, or someone, changed it): fetch all again
			for k := range in.bits {
				in.bits[k] = 0
			}
			in.have = 0
			in.dirty = true
		}
		in.mu.Unlock()
		if again {
			s.log.Warn("FileXfer: finished file does not match, fetching it again", "id", in.rec.ID)
			return false
		}
		s.failIncomingLocked(in, errors.New("the file does not match what was offered"))
		return true
	}
	in.fmu.Lock()
	_ = f.Sync()
	_ = f.Close()
	in.f = nil
	in.fmu.Unlock()

	in.mu.Lock()
	part, final := in.rec.Part, in.rec.Path
	in.mu.Unlock()
	dir := filepath.Dir(final)
	if _, err := os.Lstat(final); err == nil {
		if final, err = freeName(dir, in.rec.Name); err != nil {
			s.failIncomingLocked(in, err)
			return true
		}
	}
	if err := os.Rename(part, final); err != nil {
		s.failIncomingLocked(in, err)
		return true
	}
	markFromInternet(final)
	in.mu.Lock()
	in.rec.Path = final
	in.mu.Unlock()
	s.endIncomingLocked(in, StateDone, "")
	_ = s.opt.Store.Delete(append(append([]byte(nil), bitsPrefix...), in.rec.ID...))
	go func() {
		for k := 0; k < 3; k++ { // the sender marks the transfer delivered; a lost frame is no harm
			s.sendUp(in, kindDone, nil)
			time.Sleep(time.Second)
		}
	}()
	s.log.Info("FileXfer: file received", "id", in.rec.ID, "size", in.rec.Size)
	return true
}

// fetch is the receiver's loop: it keeps a window of chunks asked for and not yet arrived. The
// window grows while chunks come back promptly and halves when they are lost; chunks not back
// within the timeout are asked for again. It never gives up on its own: a peer that went away is
// waited for until it comes back or the user cancels.
func (s *Service) fetch(in *incoming, quit chan struct{}) {
	defer s.persist(in)
	cwnd := float64(s.opt.InitialWindow)
	ssthresh := float64(s.opt.MaxWindow)
	inflight := map[int]time.Time{}
	var retry []int
	cursor := 0
	var srtt, minRTT time.Duration
	rto := s.opt.MinRTO * 2
	lastProgress := time.Now()
	lastLoss := time.Time{}
	lastPersist := time.Now()
	lastConnect := time.Time{}
	sendFail := 0
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	nextMissing := func() int {
		for len(retry) > 0 {
			i := retry[0]
			retry = retry[1:]
			if _, busy := inflight[i]; !busy && !in.hasChunk(i) {
				return i
			}
		}
		for cursor < in.n {
			i := cursor
			cursor++
			if _, busy := inflight[i]; !busy && !in.hasChunk(i) {
				return i
			}
		}
		return -1
	}

	for {
		in.mu.Lock()
		complete := in.have >= in.n
		in.mu.Unlock()
		if complete {
			if s.finish(in) {
				return
			}
			cursor, retry, inflight = 0, nil, map[int]time.Time{}
			continue
		}

		// ask for more while the window has room
		in.mu.Lock()
		var want []int
		for len(inflight)+len(want) < int(cwnd) {
			i := nextMissing()
			if i < 0 {
				break
			}
			want = append(want, i)
		}
		if len(want) == 0 && len(inflight) == 0 && len(retry) == 0 && cursor >= in.n {
			cursor = 0 // an arrival notice was lost: look over the whole file again
		}
		in.mu.Unlock()
		if len(want) > 0 {
			now := time.Now()
			ok := true
			for len(want) > 0 {
				batch := want
				if len(batch) > maxWant {
					batch = batch[:maxWant]
				}
				want = want[len(batch):]
				if !s.sendUp(in, kindWant, encodeWant(batch)) {
					ok = false
					retry = append(retry, batch...)
					retry = append(retry, want...)
					break
				}
				for _, i := range batch {
					inflight[i] = now
				}
			}
			if !ok {
				sendFail++
				// the peer cannot be reached at all: try to connect, and wait before asking again
				if s.opt.Connect != nil && time.Since(lastConnect) > 20*time.Second {
					lastConnect = time.Now()
					go func() {
						ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
						defer cancel()
						_ = s.opt.Connect(ctx, in.peer)
					}()
				}
				wait := time.Duration(min(sendFail, 10)) * 500 * time.Millisecond
				select {
				case <-quit:
					return
				case <-s.ctx.Done():
					return
				case <-time.After(wait):
				}
			} else {
				sendFail = 0
			}
		}

		select {
		case <-quit:
			return
		case <-s.ctx.Done():
			return
		case i := <-in.arrived:
			now := time.Now()
			got := func(i int) {
				t0, ok := inflight[i]
				if !ok {
					return
				}
				delete(inflight, i)
				rtt := now.Sub(t0)
				if minRTT == 0 || rtt < minRTT {
					minRTT = rtt
				}
				if srtt == 0 {
					srtt = rtt
				} else {
					srtt = (7*srtt + rtt) / 8
				}
				rto = max(s.opt.MinRTO, 3*srtt)
				// grow while the chunks do not just queue up behind each other
				if srtt < 2*minRTT+250*time.Millisecond {
					if cwnd < ssthresh {
						cwnd++
					} else {
						cwnd += 1 / cwnd
					}
					cwnd = min(cwnd, float64(s.opt.MaxWindow))
				}
			}
			got(i)
			for more := true; more; {
				select {
				case j := <-in.arrived:
					got(j)
				default:
					more = false
				}
			}
			lastProgress = now
			in.mu.Lock()
			wasStalled := in.stalled
			in.stalled = false
			emit := in.meter.due() || wasStalled
			in.mu.Unlock()
			if emit {
				s.emit(in.info())
			}
		case <-tick.C:
			now := time.Now()
			lost := false
			for i, t0 := range inflight {
				if now.Sub(t0) > rto {
					delete(inflight, i)
					retry = append(retry, i)
					lost = true
				}
			}
			if lost && now.Sub(lastLoss) > max(srtt, 200*time.Millisecond) {
				ssthresh = max(cwnd/2, 2)
				cwnd = ssthresh
				lastLoss = now
				rto = min(rto*2, 30*time.Second) // back off while chunks keep getting lost
			}
			if now.Sub(lastPersist) > time.Second {
				lastPersist = now
				s.persist(in)
			}
			if now.Sub(lastProgress) > s.opt.StallAfter {
				in.mu.Lock()
				first := !in.stalled
				in.stalled = true
				in.mu.Unlock()
				if first {
					s.emit(in.info())
				}
				if s.opt.Connect != nil && now.Sub(lastConnect) > 30*time.Second {
					lastConnect = now
					go func() {
						ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
						defer cancel()
						_ = s.opt.Connect(ctx, in.peer)
					}()
				}
			}
			in.mu.Lock()
			emit := in.meter.due()
			in.mu.Unlock()
			if emit {
				s.emit(in.info())
			}
		}
	}
}

// ---- frames and the rest -----------------------------------------------------------------------

// HandleFrame takes a transfer frame that arrived directly or over a relay circuit. Who sent it is
// not trusted: a frame counts only if it opens with the key of the transfer it names.
func (s *Service) HandleFrame(frame []byte) {
	kind, id, ok := parseHeader(frame)
	if !ok {
		s.drop("header")
		return
	}
	s.mu.Lock()
	in, o := s.in[id], s.out[id]
	s.mu.Unlock()
	switch kind {
	case kindChunk:
		if in != nil {
			s.handleChunk(in, frame)
		}
	case kindWant:
		if o != nil {
			s.handleWant(o, frame)
		}
	case kindDone:
		if o != nil {
			s.handleFromReceiver(o, kind, frame)
		}
	case kindCancel:
		// a cancel opens with only one of the two keys, which tells its direction
		if in != nil {
			if _, err := open(in.k.toReceiver, frame); err == nil {
				s.handleFromSender(in, frame)
				return
			}
		}
		if o != nil {
			s.handleFromReceiver(o, kind, frame)
		}
	default:
		s.drop("kind")
	}
}

func (s *Service) sendFrame(peer types.PeerID, frame []byte) bool {
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	defer cancel()
	return s.opt.Transport.Send(ctx, peer, frame) == nil
}

// sendUp sends a frame from the receiver to the sender. A group member puts its own ID first: the
// sender serves every member under the same key and must know whom to answer.
func (s *Service) sendUp(in *incoming, kind frameKind, body []byte) bool {
	if in.rec.Group != "" {
		body = append(append(make([]byte, 0, len(s.opt.Self)+len(body)), s.opt.Self[:]...), body...)
	}
	return s.sendFrame(in.peer, seal(in.k.toSender, kind, in.id, body))
}

func (s *Service) sendCancel(peer types.PeerID, aead cipher.AEAD, id [idLen]byte, reason byte) {
	s.sendFrame(peer, seal(aead, kindCancel, id, []byte{reason}))
}

func (s *Service) drop(why string) {
	v, _ := s.dropped.LoadOrStore(why, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// Dropped is how many frames were thrown away for the given reason ("open", "proof", "chunk"...).
func (s *Service) Dropped(why string) int64 {
	v, ok := s.dropped.Load(why)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

// Get is one transfer.
func (s *Service) Get(id string) (Info, error) {
	key, ok := idKey(id)
	if !ok {
		return Info{}, errNotFound
	}
	s.mu.Lock()
	in, o := s.in[key], s.out[key]
	s.mu.Unlock()
	switch {
	case in != nil:
		return in.info(), nil
	case o != nil:
		return o.info(), nil
	}
	return Info{}, errNotFound
}

// List is every transfer with peer (all of them for the zero ID), oldest first.
func (s *Service) List(peer types.PeerID) []Info {
	s.mu.Lock()
	ins := make([]*incoming, 0, len(s.in))
	outs := make([]*outgoing, 0, len(s.out))
	for _, in := range s.in {
		if peer == (types.PeerID{}) || in.peer == peer {
			ins = append(ins, in)
		}
	}
	for _, o := range s.out {
		if peer == (types.PeerID{}) || o.peer == peer {
			outs = append(outs, o)
		}
	}
	s.mu.Unlock()
	out := make([]Info, 0, len(ins)+len(outs))
	for _, in := range ins {
		out = append(out, in.info())
	}
	for _, o := range outs {
		out = append(out, o.info())
	}
	sortInfos(out)
	return out
}

func (s *Service) emit(i Info) {
	if s.opt.Emit != nil {
		s.opt.Emit(i)
	}
}

func idKey(id string) ([idLen]byte, bool) {
	var k [idLen]byte
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != idLen {
		return k, false
	}
	copy(k[:], raw)
	return k, true
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
