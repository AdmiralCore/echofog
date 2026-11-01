package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4/options"
	"github.com/dgraph-io/badger/v4/pb"
	"github.com/dgraph-io/badger/v4/table"
	"github.com/dgraph-io/badger/v4/y"
	"github.com/klauspost/compress/snappy"
	"google.golang.org/protobuf/proto"
)

// A table whose end never reached the disk is the other way badger dies on open. A table is its
// blocks, one after the other, then its index (where each block starts, its smallest key, the
// bloom filter) and a footer with the index's length and checksum. When the process or the
// machine goes down while the table's last pages are still in memory, those pages read as zeros:
// the blocks are there, the index is not, and badger panics reading it, in a goroutine nobody can
// recover from (the app closes right after the password). Every block carries its own checksum, so
// RepairBrokenTables finds the blocks one by one from the start, reads their entries back and
// writes the table again with badger's own builder, under the same name, so the MANIFEST stays as
// it is. What is lost is the blocks the zeros reached, if any; the writes since the table was made
// are in the memtables' logs and come back when badger replays them.

const maxBlockLen = 1 << 20 // a block is about BlockSize (4 KiB) before compression

// SalvageOptions are the badger options the database was written with that a rebuilt table needs.
type SalvageOptions struct {
	BaseTableSize int64
	BlockSize     int
	Compression   options.CompressionType
	BloomFalsePos float64
}

// RepairBrokenTables rebuilds the tables of dir whose index cannot be read. It returns their IDs
// and where the copy of the database before the repair is (empty when nothing was wrong). A table
// with nothing left to read is removed from the MANIFEST, as RepairUnwrittenTables does.
func RepairBrokenTables(dir string, o SalvageOptions) ([]uint64, string, error) {
	broken, err := brokenTables(dir)
	if err != nil || len(broken) == 0 {
		return nil, "", err
	}
	backup := dir + ".backup-" + time.Now().Format("20060102-150405")
	if err := copyDir(dir, backup); err != nil {
		return nil, "", fmt.Errorf("keeping a copy before the repair: %w", err)
	}
	var empty []uint64
	for _, id := range broken {
		name := filepath.Join(dir, fmt.Sprintf("%06d.sst", id))
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, backup, err
		}
		entries := salvageEntries(data, o.Compression)
		if err := os.Rename(name, name+".broken"); err != nil {
			return nil, backup, err
		}
		if len(entries) == 0 {
			empty = append(empty, id)
			continue
		}
		if err := writeTable(name, entries, o); err != nil {
			return nil, backup, fmt.Errorf("rebuilding table %d: %w", id, err)
		}
	}
	if len(empty) > 0 {
		levels, err := manifestLevels(filepath.Join(dir, "MANIFEST"))
		if err != nil {
			return nil, backup, err
		}
		set := &pb.ManifestChangeSet{}
		for _, id := range empty {
			if lvl, ok := levels[id]; ok {
				set.Changes = append(set.Changes, &pb.ManifestChange{Id: id, Op: pb.ManifestChange_DELETE, Level: lvl})
			}
		}
		if len(set.Changes) > 0 {
			if err := appendManifest(filepath.Join(dir, "MANIFEST"), set); err != nil {
				return nil, backup, err
			}
		}
	}
	return broken, backup, nil
}

// brokenTables lists the .sst files whose footer or index does not check out (all-zero ones are
// RepairUnwrittenTables').
func brokenTables(dir string) ([]uint64, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []uint64
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.HasSuffix(name, ".sst") {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSuffix(name, ".sst"), 10, 64)
		if err != nil {
			continue
		}
		ok, err := tableIndexReadable(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if !ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// tableIndexReadable reads a table's end only (its index and footer), not its blocks: it runs at
// every start, over every table.
func tableIndexReadable(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := st.Size()
	tail := int64(64 << 10) // the index and the bloom filter: a few KiB for an 8 MiB table
	for {
		if tail > size {
			tail = size
		}
		buf := make([]byte, tail)
		if _, err := f.ReadAt(buf, size-tail); err != nil {
			return false, err
		}
		if indexReadable(buf) {
			return true, nil
		}
		if tail == size || !indexFits(buf) {
			return false, nil
		}
		tail *= 4 // a bigger index than the tail read: read more
	}
}

// indexFits reports whether the footer of a table's tail says its index is longer than the tail.
func indexFits(tail []byte) bool {
	n := len(tail)
	if n < 8 {
		return false
	}
	cl := int(binary.BigEndian.Uint32(tail[n-4:]))
	if cl <= 0 || cl > n-8 {
		return false
	}
	il := int(binary.BigEndian.Uint32(tail[n-8-cl : n-4-cl]))
	return il > 0 && n-8-cl-il < 0
}

// indexReadable checks a table's footer the way badger reads it: checksum length, checksum,
// index length, index, and the index against its checksum.
func indexReadable(data []byte) bool {
	n := len(data)
	if n < 8 {
		return false
	}
	cl := int(binary.BigEndian.Uint32(data[n-4:]))
	if cl <= 0 || cl > n-8 {
		return false
	}
	var chk pb.Checksum
	if proto.Unmarshal(data[n-4-cl:n-4], &chk) != nil {
		return false
	}
	il := int(binary.BigEndian.Uint32(data[n-8-cl : n-4-cl]))
	start := n - 8 - cl - il
	if il <= 0 || start < 0 {
		return false
	}
	return y.VerifyChecksum(data[start:n-8-cl], &chk) == nil
}

type salvaged struct {
	key []byte
	vs  y.ValueStruct
}

// salvageEntries reads a table's blocks from its start for as long as each checks out.
func salvageEntries(data []byte, c options.CompressionType) []salvaged {
	var out []salvaged
	pos := 0
	for pos < len(data) {
		blk, next, ok := nextBlock(data, pos, c)
		if !ok {
			break
		}
		entries, ok := blockEntries(blk)
		if !ok {
			break
		}
		out = append(out, entries...)
		pos = next
	}
	return out
}

// nextBlock is the block that starts at pos, decompressed, with where the next one starts.
func nextBlock(data []byte, pos int, c options.CompressionType) ([]byte, int, bool) {
	switch c {
	case options.Snappy:
		end, ok := snappyEnd(data, pos)
		if !ok {
			return nil, 0, false
		}
		blk, err := snappy.Decode(nil, data[pos:end])
		if err != nil || !blockChecks(blk) {
			return nil, 0, false
		}
		return blk, end, true
	case options.None:
		// no length anywhere: the block ends where a checksum over what precedes it checks out
		for end := pos + 12; end <= len(data) && end-pos <= maxBlockLen; end++ {
			if blockChecks(data[pos:end]) {
				return data[pos:end], end, true
			}
		}
	}
	return nil, 0, false
}

// snappyEnd walks a snappy block that starts at pos (a varint length, then literals and copies)
// and returns where it ends.
func snappyEnd(data []byte, pos int) (int, bool) {
	want, n := binary.Uvarint(data[pos:])
	if n <= 0 || want == 0 || want > maxBlockLen {
		return 0, false
	}
	s, got := pos+n, uint64(0)
	for got < want {
		if s >= len(data) {
			return 0, false
		}
		tag := data[s]
		switch tag & 3 {
		case 0: // literal
			l := int(tag >> 2)
			s++
			if l >= 60 {
				extra := l - 59
				if s+extra > len(data) {
					return 0, false
				}
				l = 0
				for i := 0; i < extra; i++ {
					l |= int(data[s+i]) << (8 * i)
				}
				s += extra
			}
			l++
			s += l
			got += uint64(l)
		case 1:
			got += uint64(4 + (tag>>2)&7)
			s += 2
		case 2:
			got += uint64(1 + tag>>2)
			s += 3
		case 3:
			got += uint64(1 + tag>>2)
			s += 5
		}
	}
	if got != want || s > len(data) {
		return 0, false
	}
	return s, true
}

// blockChecks verifies a decompressed block against the checksum at its end.
func blockChecks(blk []byte) bool {
	n := len(blk)
	if n < 8 {
		return false
	}
	cl := int(binary.BigEndian.Uint32(blk[n-4:]))
	if cl <= 0 || cl > n-8 {
		return false
	}
	var chk pb.Checksum
	if proto.Unmarshal(blk[n-4-cl:n-4], &chk) != nil {
		return false
	}
	return y.VerifyChecksum(blk[:n-4-cl], &chk) == nil
}

// blockEntries reads the entries of a checked block: each a header (overlap with the block's
// first key, length of the rest), the rest of the key, and the value.
func blockEntries(blk []byte) ([]salvaged, bool) {
	n := len(blk)
	cl := int(binary.BigEndian.Uint32(blk[n-4:]))
	end := n - 4 - cl // just past the number of entries
	count := int(binary.BigEndian.Uint32(blk[end-4 : end]))
	idx := end - 4 - count*4
	if count <= 0 || idx < 0 {
		return nil, false
	}
	offsets := make([]int, count)
	for i := range offsets {
		// the offsets are written as they lie in memory (y.U32SliceToBytes), the count big-endian
		offsets[i] = int(binary.LittleEndian.Uint32(blk[idx+4*i:]))
	}
	var base []byte
	out := make([]salvaged, 0, count)
	for i, start := range offsets {
		stop := idx
		if i+1 < count {
			stop = offsets[i+1]
		}
		if start+4 > stop || stop > idx {
			return nil, false
		}
		overlap := int(binary.LittleEndian.Uint16(blk[start:]))
		diff := int(binary.LittleEndian.Uint16(blk[start+2:]))
		if start+4+diff > stop || overlap > len(base) {
			return nil, false
		}
		key := append(append([]byte(nil), base[:overlap]...), blk[start+4:start+4+diff]...)
		if i == 0 {
			base = key
		}
		var vs y.ValueStruct
		vs.Decode(append([]byte(nil), blk[start+4+diff:stop]...))
		out = append(out, salvaged{key: key, vs: vs})
	}
	return out, true
}

// writeTable writes entries (in the order they were read: sorted) as a table at name.
func writeTable(name string, entries []salvaged, o SalvageOptions) error {
	b := table.NewTableBuilder(table.Options{
		TableSize:          uint64(o.BaseTableSize),
		BlockSize:          o.BlockSize,
		BloomFalsePositive: o.BloomFalsePos,
		ChkMode:            options.NoVerification,
		Compression:        o.Compression,
	})
	defer b.Close()
	for _, e := range entries {
		var vlen uint32
		if e.vs.Meta&bitValuePointer != 0 && len(e.vs.Value) >= 8 {
			vlen = binary.BigEndian.Uint32(e.vs.Value[4:8]) // the value's length in the value log
		}
		b.Add(e.key, e.vs, vlen)
	}
	t, err := table.CreateTable(name, b)
	if err != nil {
		return err
	}
	if t == nil {
		return errors.New("no table written")
	}
	// closed, not DecrRef'd: the last reference going deletes the file
	return t.MmapFile.Close(-1)
}

// bitValuePointer is badger's mark of a value kept in the value log (a pointer in the table).
const bitValuePointer byte = 1 << 1
