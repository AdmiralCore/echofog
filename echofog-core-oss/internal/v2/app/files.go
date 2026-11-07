package app

import (
	"context"
	"errors"
	"sync"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/files"
	"github.com/echofog/echofog-core/internal/v2/mux"
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

type fileState struct {
	mu sync.Mutex
}

func (a *App) stopFiles() {}

var errNotYet = errors.New("app: not implemented")

// SendFile offers the file at path to a contact.
func (a *App) SendFile(ctx context.Context, peerHex, path string) (Transfer, error) {
	return Transfer{}, errNotYet
}

// SendGroupFile offers the file at path to the members of a group.
func (a *App) SendGroupFile(ctx context.Context, group string, members [][32]byte, path string) (Transfer, error) {
	return Transfer{}, errNotYet
}

// OnFileOffer takes a FILE message the chat layer delivered; from is the sender it verified. group is
// "" for a direct message (the sender must be a contact) or the group the message came in, where the
// caller has checked that the sender is a member.
func (a *App) OnFileOffer(from [32]byte, group string, m *chatpb.PersonalMessageProto) error {
	return errNotYet
}

// AcceptFile starts (or resumes) the download of an offered file.
func (a *App) AcceptFile(id string) (Transfer, error) { return Transfer{}, errNotYet }

// DeclineFile refuses an offered file.
func (a *App) DeclineFile(id string) error { return errNotYet }

// CancelFile stops a transfer and drops what was downloaded of it.
func (a *App) CancelFile(id string) error { return errNotYet }

// GetFileTransfer returns one transfer.
func (a *App) GetFileTransfer(id string) (Transfer, error) { return Transfer{}, errNotYet }

// ListFileTransfers lists the transfers with a contact ("" for all), newest first.
func (a *App) ListFileTransfers(peerHex string) ([]Transfer, error) { return nil, errNotYet }

// FileTransferPath is the file of a finished transfer.
func (a *App) FileTransferPath(id string) (string, error) { return "", errNotYet }

// DownloadDir is where received files go.
func (a *App) DownloadDir() string { return "" }

// OnFileTransfer calls cb whenever a transfer changes; the returned function stops it.
func (a *App) OnFileTransfer(cb func(Transfer)) func() { return func() {} }

// FileServed records that a recipient confirmed the whole file (files.ServerConfig.OnDone).
func (a *App) FileServed(fid [16]byte, node [32]byte) {}

// VoiceSent is the result of SendVoice.
type VoiceSent struct {
	ID       string
	Transfer *Transfer // the file of a long clip; nil for a clip inside the message
}

// SendVoice sends a recording to a contact as a voice message.
func (a *App) SendVoice(ctx context.Context, peerHex string, rec voice.Recording) (VoiceSent, error) {
	return VoiceSent{}, errNotYet
}

// OnVoice takes a VOICE message the chat layer delivered; from is the sender it verified.
func (a *App) OnVoice(from [32]byte, m *chatpb.PersonalMessageProto) (string, error) {
	return "", errNotYet
}

// VoiceAudio is the clean clip of a received voice message, once there is one.
func (a *App) VoiceAudio(id string) ([]byte, bool) { return nil, false }
