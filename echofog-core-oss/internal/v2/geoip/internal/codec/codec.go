// Package codec reads and writes the compact binary country database embedded by package geoip.
//
// The format is deliberately small and needs no third-party code. All integers are unsigned
// base-128 varints unless stated otherwise:
//
//	header:  "EFGI" (4 bytes), version 1 (1 byte), three zero bytes
//	country table: count, then count pairs of ASCII letters
//	IPv4 section
//	IPv6 section
//
// A section holds: record count, block size, block count, record bytes, a block index and the
// records. A block index entry is the first start address of a block (4 bytes for IPv4, 16 for
// IPv6) and a big-endian uint32 offset of the block's first record relative to the record bytes.
// A record is: start delta from the previous record's end, range length, one-byte country index.
// The first delta is relative to zero. Blocks of blockSize records let a lookup binary-search
// the index and scan at most one block.
//
// Encode merges adjacent ranges of the same country and rejects overlaps, so the writer and the
// generator cannot silently produce contradictory data.
package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"sort"
)

const (
	// Magic starts every database.
	Magic = "EFGI"
	// Version is the only format version this package reads and writes.
	Version = 1

	headerLen    = 8
	blockSize    = 64
	maxCountries = 255
)

// Record is one inclusive address range and its ISO 3166-1 alpha-2 country.
type Record struct {
	Start   netip.Addr
	End     netip.Addr
	Country string
}

// Encode writes the database for records to w. Records may be in any order, but must not
// overlap and must not use the "ZZ" code (callers drop rows with an unknown country).
func Encode(w io.Writer, records []Record) error {
	var v4, v6 []Record
	codes := make(map[string]struct{})
	for i, r := range records {
		if err := validateRecord(r); err != nil {
			return fmt.Errorf("record %d: %w", i, err)
		}
		codes[r.Country] = struct{}{}
		if r.Start.Is4() {
			v4 = append(v4, r)
		} else {
			v6 = append(v6, r)
		}
	}

	var err error
	if v4, err = normalize(v4); err != nil {
		return fmt.Errorf("ipv4: %w", err)
	}
	if v6, err = normalize(v6); err != nil {
		return fmt.Errorf("ipv6: %w", err)
	}

	names := make([]string, 0, len(codes))
	for code := range codes {
		names = append(names, code)
	}
	sort.Strings(names)
	if len(names) > maxCountries {
		return fmt.Errorf("%d countries, at most %d fit the one-byte index", len(names), maxCountries)
	}
	index := make(map[string]byte, len(names))
	for i, code := range names {
		index[code] = byte(i)
	}

	v4Section, err := encodeFamily(v4, index, 4)
	if err != nil {
		return fmt.Errorf("ipv4: %w", err)
	}
	v6Section, err := encodeFamily(v6, index, 16)
	if err != nil {
		return fmt.Errorf("ipv6: %w", err)
	}

	var out bytes.Buffer
	out.WriteString(Magic)
	out.WriteByte(Version)
	out.Write([]byte{0, 0, 0})
	out.Write(binary.AppendUvarint(nil, uint64(len(names))))
	for _, code := range names {
		out.WriteString(code)
	}
	out.Write(v4Section)
	out.Write(v6Section)
	_, err = w.Write(out.Bytes())
	return err
}

func validateRecord(r Record) error {
	if !r.Start.IsValid() || !r.End.IsValid() {
		return errors.New("invalid address")
	}
	if r.Start.Is4() != r.End.Is4() {
		return errors.New("range mixes address families")
	}
	if r.End.Less(r.Start) {
		return errors.New("end is before start")
	}
	if len(r.Country) != 2 {
		return fmt.Errorf("country %q is not two letters", r.Country)
	}
	for i := 0; i < 2; i++ {
		if c := r.Country[i]; c < 'A' || c > 'Z' {
			return fmt.Errorf("country %q is not two uppercase letters", r.Country)
		}
	}
	if r.Country == "ZZ" {
		return errors.New(`"ZZ" is not a country; drop unknown ranges`)
	}
	return nil
}

// normalize sorts ranges by start and merges adjacent ranges of the same country.
func normalize(in []Record) ([]Record, error) {
	sort.Slice(in, func(i, j int) bool { return in[i].Start.Less(in[j].Start) })
	out := in[:0]
	for _, r := range in {
		if len(out) > 0 {
			last := &out[len(out)-1]
			if !last.End.Less(r.Start) {
				return nil, fmt.Errorf("overlapping ranges %s-%s and %s-%s",
					last.Start, last.End, r.Start, r.End)
			}
			if last.Country == r.Country && last.End.Next() == r.Start {
				last.End = r.End
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

type blockIndex struct {
	startHi, startLo uint64
	off              uint32
}

func encodeFamily(records []Record, index map[string]byte, addrSize int) ([]byte, error) {
	var rec bytes.Buffer
	blocks := make([]blockIndex, 0, (len(records)+blockSize-1)/blockSize)
	var prevHi, prevLo uint64
	for i, r := range records {
		startHi, startLo := addrParts(r.Start)
		endHi, endLo := addrParts(r.End)
		if i%blockSize == 0 {
			if rec.Len() > math.MaxUint32 {
				return nil, errors.New("record section exceeds 4 GiB")
			}
			blocks = append(blocks, blockIndex{startHi: startHi, startLo: startLo, off: uint32(rec.Len())})
		}
		var dHi, dLo uint64
		if i > 0 {
			dHi, dLo = sub128(startHi, startLo, prevHi, prevLo)
		} else {
			dHi, dLo = startHi, startLo
		}
		lHi, lLo := sub128(endHi, endLo, startHi, startLo)
		if addrSize == 4 {
			rec.Write(binary.AppendUvarint(nil, dLo))
			rec.Write(binary.AppendUvarint(nil, lLo))
		} else {
			rec.Write(appendU128(nil, dHi, dLo))
			rec.Write(appendU128(nil, lHi, lLo))
		}
		ci, ok := index[r.Country]
		if !ok {
			return nil, fmt.Errorf("country %q is missing from the table", r.Country)
		}
		rec.WriteByte(ci)
		prevHi, prevLo = endHi, endLo
	}

	var out bytes.Buffer
	out.Write(binary.AppendUvarint(nil, uint64(len(records))))
	out.Write(binary.AppendUvarint(nil, blockSize))
	out.Write(binary.AppendUvarint(nil, uint64(len(blocks))))
	out.Write(binary.AppendUvarint(nil, uint64(rec.Len())))
	for _, b := range blocks {
		if addrSize == 4 {
			var a [4]byte
			binary.BigEndian.PutUint32(a[:], uint32(b.startLo))
			out.Write(a[:])
		} else {
			var a [16]byte
			binary.BigEndian.PutUint64(a[:8], b.startHi)
			binary.BigEndian.PutUint64(a[8:], b.startLo)
			out.Write(a[:])
		}
		var off [4]byte
		binary.BigEndian.PutUint32(off[:], b.off)
		out.Write(off[:])
	}
	out.Write(rec.Bytes())
	return out.Bytes(), nil
}

// DB is a decoded database. It keeps the original bytes and an index; lookups allocate nothing.
type DB struct {
	countries []string
	v4        family
	v6        family
}

type family struct {
	blocks  []blockIndex
	records []byte
}

// Decode validates and indexes an encoded database. The returned DB refers to data until the
// caller drops it. Decoding never walks individual records; lookups stay bounded by block and
// section offsets, so a malformed record byte can only make a lookup return "unknown".
func Decode(data []byte) (*DB, error) {
	if len(data) < headerLen {
		return nil, io.ErrUnexpectedEOF
	}
	if string(data[:4]) != Magic {
		return nil, errors.New("bad magic")
	}
	if data[4] != Version {
		return nil, fmt.Errorf("unsupported version %d", data[4])
	}
	if data[5] != 0 || data[6] != 0 || data[7] != 0 {
		return nil, errors.New("nonzero reserved header bytes")
	}

	r := &reader{b: data, off: headerLen}
	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	if n > maxCountries {
		return nil, fmt.Errorf("%d countries, at most %d fit the one-byte index", n, maxCountries)
	}
	codes := make([]string, n)
	seen := make(map[string]struct{}, n)
	for i := range codes {
		b, err := r.take(2)
		if err != nil {
			return nil, err
		}
		code := string(b)
		if !validCountry(code) {
			return nil, fmt.Errorf("country %q is not two uppercase letters", code)
		}
		if _, dup := seen[code]; dup {
			return nil, fmt.Errorf("duplicate country %q", code)
		}
		seen[code] = struct{}{}
		codes[i] = code
	}

	db := &DB{countries: codes}
	if db.v4, err = decodeFamily(r, 4); err != nil {
		return nil, fmt.Errorf("ipv4: %w", err)
	}
	if db.v6, err = decodeFamily(r, 16); err != nil {
		return nil, fmt.Errorf("ipv6: %w", err)
	}
	if r.off != len(data) {
		return nil, fmt.Errorf("%d trailing bytes", len(data)-r.off)
	}
	return db, nil
}

func validCountry(code string) bool {
	if len(code) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		if c := code[i]; c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

type reader struct {
	b   []byte
	off int
}

func (r *reader) uvarint() (uint64, error) {
	if r.off >= len(r.b) {
		return 0, io.ErrUnexpectedEOF
	}
	v, n := binary.Uvarint(r.b[r.off:])
	if n <= 0 {
		return 0, errors.New("invalid varint")
	}
	r.off += n
	return v, nil
}

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || n > len(r.b)-r.off {
		return nil, io.ErrUnexpectedEOF
	}
	b := r.b[r.off : r.off+n]
	r.off += n
	return b, nil
}

func decodeFamily(r *reader, addrSize int) (family, error) {
	var f family
	count, err := r.uvarint()
	if err != nil {
		return f, err
	}
	if count > uint64(len(r.b)) {
		return f, errors.New("record count exceeds the database size")
	}
	bs, err := r.uvarint()
	if err != nil {
		return f, err
	}
	if bs == 0 || bs > 1<<16 {
		return f, fmt.Errorf("block size %d is out of range", bs)
	}
	nBlocks, err := r.uvarint()
	if err != nil {
		return f, err
	}
	if nBlocks != (count+bs-1)/bs {
		return f, fmt.Errorf("%d blocks for %d records of %d", nBlocks, count, bs)
	}
	recLen, err := r.uvarint()
	if err != nil {
		return f, err
	}
	entrySize := uint64(addrSize + 4)
	if nBlocks > uint64(len(r.b)-r.off)/entrySize {
		return f, io.ErrUnexpectedEOF
	}
	indexLen := nBlocks * entrySize
	index, err := r.take(int(indexLen))
	if err != nil {
		return f, err
	}
	if recLen > uint64(len(r.b)-r.off) {
		return f, io.ErrUnexpectedEOF
	}
	records, err := r.take(int(recLen))
	if err != nil {
		return f, err
	}

	f.blocks = make([]blockIndex, nBlocks)
	for i := range f.blocks {
		e := index[i*int(entrySize):]
		if addrSize == 4 {
			f.blocks[i].startLo = uint64(binary.BigEndian.Uint32(e[:4]))
		} else {
			f.blocks[i].startHi = binary.BigEndian.Uint64(e[:8])
			f.blocks[i].startLo = binary.BigEndian.Uint64(e[8:16])
		}
		f.blocks[i].off = binary.BigEndian.Uint32(e[addrSize:])
		if uint64(f.blocks[i].off) >= recLen {
			return f, fmt.Errorf("block %d starts past the records", i)
		}
		if i > 0 {
			prev := f.blocks[i-1]
			if f.blocks[i].off <= prev.off {
				return f, fmt.Errorf("block %d offset is not increasing", i)
			}
			if !less128(prev.startHi, prev.startLo, f.blocks[i].startHi, f.blocks[i].startLo) {
				return f, fmt.Errorf("block %d start is not increasing", i)
			}
		} else if f.blocks[i].off != 0 {
			return f, errors.New("first block does not start at offset 0")
		}
	}
	f.records = records
	return f, nil
}

// Lookup returns the country of a address, or false when the database has no range for it.
// IPv4-mapped IPv6 addresses are resolved as IPv4.
func (db *DB) Lookup(a netip.Addr) (string, bool) {
	if !a.IsValid() {
		return "", false
	}
	a = a.Unmap()
	if a.Is4() {
		return db.lookup4(a)
	}
	return db.lookup6(a)
}

func (db *DB) lookup4(a netip.Addr) (string, bool) {
	b := a.As4()
	key := uint64(binary.BigEndian.Uint32(b[:]))
	blocks := db.v4.blocks
	i := sort.Search(len(blocks), func(i int) bool { return blocks[i].startLo > key }) - 1
	if i < 0 {
		return "", false
	}
	rec := db.v4.records
	off := int(blocks[i].off)
	end := len(rec)
	if i+1 < len(blocks) {
		end = int(blocks[i+1].off)
	}
	cur := blocks[i].startLo
	for first := true; off < end; first = false {
		delta, n := binary.Uvarint(rec[off:end])
		if n <= 0 {
			return "", false
		}
		off += n
		length, n := binary.Uvarint(rec[off:end])
		if n <= 0 {
			return "", false
		}
		off += n
		if off >= end {
			return "", false
		}
		ci := rec[off]
		off++
		if !first {
			if cur > math.MaxUint64-delta {
				return "", false
			}
			cur += delta
		}
		if key < cur {
			return "", false
		}
		if cur > math.MaxUint64-length {
			return "", false
		}
		next := cur + length
		if key <= next {
			if int(ci) >= len(db.countries) {
				return "", false
			}
			return db.countries[ci], true
		}
		cur = next
	}
	return "", false
}

func (db *DB) lookup6(a netip.Addr) (string, bool) {
	b := a.As16()
	keyHi := binary.BigEndian.Uint64(b[:8])
	keyLo := binary.BigEndian.Uint64(b[8:])
	blocks := db.v6.blocks
	i := sort.Search(len(blocks), func(i int) bool {
		return less128(keyHi, keyLo, blocks[i].startHi, blocks[i].startLo)
	}) - 1
	if i < 0 {
		return "", false
	}
	rec := db.v6.records
	off := int(blocks[i].off)
	end := len(rec)
	if i+1 < len(blocks) {
		end = int(blocks[i+1].off)
	}
	curHi, curLo := blocks[i].startHi, blocks[i].startLo
	for first := true; off < end; first = false {
		dHi, dLo, n, ok := readU128(rec[off:end])
		if !ok {
			return "", false
		}
		off += n
		lHi, lLo, n, ok := readU128(rec[off:end])
		if !ok {
			return "", false
		}
		off += n
		if off >= end {
			return "", false
		}
		ci := rec[off]
		off++
		if !first {
			curHi, curLo, ok = add128(curHi, curLo, dHi, dLo)
			if !ok {
				return "", false
			}
		}
		if less128(keyHi, keyLo, curHi, curLo) {
			return "", false
		}
		nextHi, nextLo, ok := add128(curHi, curLo, lHi, lLo)
		if !ok {
			return "", false
		}
		if !less128(nextHi, nextLo, keyHi, keyLo) {
			if int(ci) >= len(db.countries) {
				return "", false
			}
			return db.countries[ci], true
		}
		curHi, curLo = nextHi, nextLo
	}
	return "", false
}

func addrParts(a netip.Addr) (hi, lo uint64) {
	if a.Is4() {
		b := a.As4()
		return 0, uint64(binary.BigEndian.Uint32(b[:]))
	}
	b := a.As16()
	return binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
}

func less128(aHi, aLo, bHi, bLo uint64) bool {
	return aHi < bHi || (aHi == bHi && aLo < bLo)
}

func sub128(aHi, aLo, bHi, bLo uint64) (hi, lo uint64) {
	lo = aLo - bLo
	var borrow uint64
	if aLo < bLo {
		borrow = 1
	}
	hi = aHi - bHi - borrow
	return hi, lo
}

func add128(aHi, aLo, bHi, bLo uint64) (hi, lo uint64, ok bool) {
	lo = aLo + bLo
	if lo < aLo {
		bHi++
		if bHi == 0 {
			return 0, 0, false
		}
	}
	hi = aHi + bHi
	if hi < aHi {
		return 0, 0, false
	}
	return hi, lo, true
}

func appendU128(dst []byte, hi, lo uint64) []byte {
	for hi != 0 || lo >= 0x80 {
		dst = append(dst, byte(lo)|0x80)
		lo = lo>>7 | hi<<57
		hi >>= 7
	}
	return append(dst, byte(lo))
}

// readU128 decodes an unsigned 128-bit varint. n is the number of bytes consumed.
func readU128(b []byte) (hi, lo uint64, n int, ok bool) {
	var shift uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		v := uint64(c & 0x7f)
		if i == 18 {
			if c >= 0x80 || v > 3 {
				return 0, 0, 0, false
			}
		}
		if shift < 64 {
			lo |= v << shift
			if shift > 57 {
				hi |= v >> (64 - shift)
			}
		} else {
			hi |= v << (shift - 64)
		}
		shift += 7
		if c < 0x80 {
			return hi, lo, i + 1, true
		}
	}
	return 0, 0, 0, false
}
