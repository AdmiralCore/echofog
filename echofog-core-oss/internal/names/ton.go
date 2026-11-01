package names

import (
	"context"
	"errors"
	"fmt"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/dns"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// On TON the record is a dns_text record (TEP-81) under the category sha256("echofog"), next to
// the name's "wallet" and "site" records. .ton names and Telegram usernames (.t.me) both answer
// dnsresolve, so one resolver reads both. The owner is the name's NFT owner.
//
//	dns_text#1eda _:Text = DNSRecord;
//	text$_ chunks:(## 8) rest:(TextChunks chunks) = Text;
//	text_chunk$_ len:(## 8) data:(bits (len * 8)) next:(TextChunkRef n) = TextChunks (n + 1);
//
// Each chunk sits in its own cell; every chunk but the last refers to the next one.

// TONCategory names the record's category; the key in the name's records is its SHA-256.
const TONCategory = "echofog"

const (
	dnsTextTag    = 0x1eda
	maxTONText    = 1024
	firstChunkMax = (1023 - 16 - 8 - 8) / 8 // the first cell also holds the tag and the chunk count
	chunkMax      = (1023 - 8) / 8
)

// TONTextRecord builds the dns_text record that holds text.
func TONTextRecord(text string) (*cell.Cell, error) {
	data := []byte(text)
	if len(data) == 0 || len(data) > maxTONText {
		return nil, fmt.Errorf("text record of %d bytes", len(data))
	}
	var chunks [][]byte
	for first := true; len(data) > 0; first = false {
		n := chunkMax
		if first {
			n = firstChunkMax
		}
		n = min(n, len(data))
		chunks = append(chunks, data[:n])
		data = data[n:]
	}
	var next *cell.Cell
	for i := len(chunks) - 1; i >= 1; i-- {
		b := cell.BeginCell().MustStoreUInt(uint64(len(chunks[i])), 8).MustStoreSlice(chunks[i], uint(len(chunks[i])*8))
		if next != nil {
			b.MustStoreRef(next)
		}
		next = b.EndCell()
	}
	b := cell.BeginCell().MustStoreUInt(dnsTextTag, 16).MustStoreUInt(uint64(len(chunks)), 8).
		MustStoreUInt(uint64(len(chunks[0])), 8).MustStoreSlice(chunks[0], uint(len(chunks[0])*8))
	if next != nil {
		b.MustStoreRef(next)
	}
	return b.EndCell(), nil
}

// ParseTONText reads a dns_text record.
func ParseTONText(rec *cell.Cell) (string, error) {
	s, err := rec.BeginParse()
	if err != nil {
		return "", err
	}
	if tag, err := s.LoadUInt(16); err != nil || tag != dnsTextTag {
		return "", errors.New("not a text record")
	}
	n, err := s.LoadUInt(8)
	if err != nil || n == 0 {
		return "", errors.New("broken text record")
	}
	var out []byte
	for i := uint64(0); i < n; i++ {
		if i > 0 {
			if s, err = s.LoadRef(); err != nil {
				return "", errors.New("broken text record: a chunk is missing")
			}
		}
		l, err := s.LoadUInt(8)
		if err != nil {
			return "", errors.New("broken text record")
		}
		data, err := s.LoadSlice(uint(l * 8))
		if err != nil {
			return "", errors.New("broken text record")
		}
		if out = append(out, data...); len(out) > maxTONText {
			return "", errors.New("text record too long")
		}
	}
	return string(out), nil
}

// TONRecordValue is the record as a name stores it (^DNSRecord) and as change_dns_record carries
// it: dns.Domain.BuildSetRecordPayload(TONCategory, value) is the message to send to the name's
// NFT from the owner's wallet.
func TONRecordValue(text string) (*cell.Cell, error) {
	rec, err := TONTextRecord(text)
	if err != nil {
		return nil, err
	}
	return cell.BeginCell().MustStoreRef(rec).EndCell(), nil
}

// TONOwner is an account as a binding stores it: the workchain byte and the 32-byte account.
func TONOwner(a *address.Address) []byte {
	return append([]byte{byte(int8(a.Workchain()))}, a.Data()...)
}

// TONEntry is what the chain says about a name.
type TONEntry struct {
	Exists bool             // the name is registered
	Record *cell.Cell       // the value under TONCategory (^DNSRecord); nil without one
	Owner  *address.Address // the name's NFT owner; nil while nobody holds it (an auction)
	Broken string           // the name's contract failed instead of answering (seen for reserved t.me names)
}

// TONSource reads names from TON. dnsSource reads liteservers; tests use a fake.
type TONSource interface {
	LookupTON(ctx context.Context, name string) (TONEntry, error)
}

type dnsSource struct{ c *dns.Client }

// NewTONSource reads names through api (the wallet's liteserver client, which checks proofs).
// root is the DNS root contract (dns.GetRootContractAddr reads it from the chain config).
func NewTONSource(api dns.TonApi, root *address.Address) TONSource {
	return dnsSource{dns.NewDNSClient(api, root)}
}

func (d dnsSource) LookupTON(ctx context.Context, name string) (TONEntry, error) {
	dom, err := d.c.Resolve(ctx, name)
	if errors.Is(err, dns.ErrNoSuchRecord) {
		return TONEntry{}, nil
	}
	var exec ton.ContractExecError
	if errors.As(err, &exec) {
		return TONEntry{Exists: true, Broken: err.Error()}, nil
	}
	if err != nil {
		return TONEntry{}, err
	}
	e := TONEntry{Exists: true, Record: dom.GetRecord(TONCategory)}
	data, err := dom.GetNFTData(ctx)
	if err != nil {
		return TONEntry{}, fmt.Errorf("owner of %s: %w", name, err)
	}
	if data.Initialized && data.OwnerAddress != nil && data.OwnerAddress.Type() == address.StdAddress {
		e.Owner = data.OwnerAddress
	}
	return e, nil
}

// readTONRecord takes the binding out of a record value (^DNSRecord).
func readTONRecord(value *cell.Cell) (Binding, error) {
	s, err := value.BeginParse()
	if err != nil {
		return Binding{}, err
	}
	rec, err := s.LoadRefCell()
	if err != nil {
		return Binding{}, errors.New("broken record: no value")
	}
	text, err := ParseTONText(rec)
	if err != nil {
		return Binding{}, err
	}
	return ParseBinding(text)
}
