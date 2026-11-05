package cards

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// State keys, one namespace under the node's store (T4.3 namespaces the whole view).
const statePrefix = "v2/node/cards/"

func seqKey() []byte    { return []byte(statePrefix + "seq") }
func issuedKey() []byte { return []byte(statePrefix + "issued") }
func currentKey(s objects.Scope) []byte {
	return []byte(statePrefix + "current/" + strconv.Itoa(int(s)))
}

// stateVersion is the format of the two local values. It is not a protocol version.
const stateVersion = 1

// maxIssuedRecords bounds a decoded map; the builder issues a few cards per week.
const maxIssuedRecords = 1 << 16

type issuedRecord struct {
	Scope objects.Scope
	At    int64
}

// load restores the seq counter, the issued-card memory and the current cards. Unreadable values
// are ignored: a forgotten seq only makes a responder send one more card, never an invalid one.
func (b *CardBuilder) load(ctx context.Context) error {
	now := b.clk.Now()

	if v, ok, err := b.kv.Get(ctx, seqKey()); err != nil {
		return fmt.Errorf("%w: read seq: %v", ErrKV, err)
	} else if ok && len(v) == 8 {
		b.seq = binary.BigEndian.Uint64(v)
	}

	if v, ok, err := b.kv.Get(ctx, issuedKey()); err != nil {
		return fmt.Errorf("%w: read issued: %v", ErrKV, err)
	} else if ok {
		if recs, err := decodeIssued(v); err == nil {
			for seq, rec := range recs {
				b.recs[seq] = rec
				if seq > b.seq {
					b.seq = seq
				}
			}
		}
	}

	devicePub, err := parseDeviceKey(b.cfg.Device)
	if err != nil {
		return err
	}
	for _, scope := range []objects.Scope{objects.ScopePublic, objects.ScopePeer} {
		v, ok, err := b.kv.Get(ctx, currentKey(scope))
		if err != nil {
			return fmt.Errorf("%w: read current %s: %v", ErrKV, scope, err)
		}
		if !ok {
			continue
		}
		fp, env, err := decodeCurrent(v)
		if err != nil {
			continue
		}
		card, err := objects.VerifyCard(env, now)
		if err != nil || card.Scope != scope || !card.DeviceKey.Equal(devicePub) {
			continue
		}
		b.cur[scope] = &currentCard{
			fingerprint: fp,
			envelope:    clone(env),
			seq:         card.Env.Seq,
			notAfter:    card.Env.NotAfter,
		}
		if card.Env.Seq > b.seq {
			b.seq = card.Env.Seq
		}
	}
	b.pruneLocked(now)
	return nil
}

// persistLocked writes the seq counter, the issued memory and the current card. The caller has
// already advanced the in-memory state, so a failed write never causes a seq to be reused. Every
// write is attempted; the seq goes first so a crash can only leave a gap, never a repeated seq.
func (b *CardBuilder) persistLocked(ctx context.Context, scope objects.Scope, seq uint64, env []byte, fp [32]byte) error {
	var errs []error

	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], seq)
	if err := b.kv.Put(ctx, seqKey(), raw[:]); err != nil {
		errs = append(errs, fmt.Errorf("%w: write seq: %v", ErrKV, err))
	}
	if recs, err := encodeIssued(b.recs); err != nil {
		errs = append(errs, err)
	} else if err := b.kv.Put(ctx, issuedKey(), recs); err != nil {
		errs = append(errs, fmt.Errorf("%w: write issued: %v", ErrKV, err))
	}
	if scope != objects.ScopeInvite {
		if err := b.kv.Put(ctx, currentKey(scope), encodeCurrent(fp, env)); err != nil {
			errs = append(errs, fmt.Errorf("%w: write current %s: %v", ErrKV, scope, err))
		}
	}
	return errors.Join(errs...)
}

// pruneLocked drops issued entries older than the handshake's memory (30 days).
func (b *CardBuilder) pruneLocked(now time.Time) {
	for seq, rec := range b.recs {
		if rec.At == 0 {
			continue
		}
		if now.Sub(time.Unix(rec.At, 0)) > handshake.IssuedMemory {
			delete(b.recs, seq)
		}
	}
}

func encodeIssued(in map[uint64]issuedRecord) ([]byte, error) {
	if len(in) > maxIssuedRecords {
		return nil, fmt.Errorf("%w: %d issued records", ErrConfig, len(in))
	}
	seqs := make([]uint64, 0, len(in))
	for seq := range in {
		seqs = append(seqs, seq)
	}
	slices.Sort(seqs)

	out := make([]byte, 0, 1+4+20*len(seqs))
	out = append(out, stateVersion)
	out = binary.BigEndian.AppendUint32(out, uint32(len(seqs)))
	for _, seq := range seqs {
		rec := in[seq]
		out = binary.BigEndian.AppendUint64(out, seq)
		out = binary.BigEndian.AppendUint32(out, uint32(rec.Scope))
		out = binary.BigEndian.AppendUint64(out, uint64(rec.At))
	}
	return out, nil
}

func decodeIssued(b []byte) (map[uint64]issuedRecord, error) {
	if len(b) < 5 || b[0] != stateVersion {
		return nil, errors.New("cards: bad issued state")
	}
	n := int(binary.BigEndian.Uint32(b[1:5]))
	if n < 0 || n > maxIssuedRecords || n > (len(b)-5)/20 || len(b) != 5+20*n {
		return nil, errors.New("cards: bad issued state")
	}
	out := make(map[uint64]issuedRecord, n)
	for i := 0; i < n; i++ {
		p := 5 + 20*i
		seq := binary.BigEndian.Uint64(b[p:])
		out[seq] = issuedRecord{
			Scope: objects.Scope(binary.BigEndian.Uint32(b[p+8:])),
			At:    int64(binary.BigEndian.Uint64(b[p+12:])),
		}
	}
	return out, nil
}

func encodeCurrent(fp [32]byte, env []byte) []byte {
	out := make([]byte, 0, 1+len(fp)+len(env))
	out = append(out, stateVersion)
	out = append(out, fp[:]...)
	out = append(out, env...)
	return out
}

func decodeCurrent(b []byte) ([32]byte, []byte, error) {
	var fp [32]byte
	if len(b) < 1+32+1 || b[0] != stateVersion {
		return fp, nil, errors.New("cards: bad current state")
	}
	copy(fp[:], b[1:33])
	return fp, clone(b[33:]), nil
}

// memoryKV is the ephemeral KV used when Config.KV is nil.
type memoryKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemoryKV() *memoryKV { return &memoryKV{m: make(map[string][]byte)} }

func (k *memoryKV) Get(_ context.Context, key []byte) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[string(key)]
	return clone(v), ok, nil
}

func (k *memoryKV) Put(_ context.Context, key, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[string(key)] = clone(value)
	return nil
}
