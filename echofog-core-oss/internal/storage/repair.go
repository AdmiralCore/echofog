package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4/pb"
	"google.golang.org/protobuf/proto"
)

// A table that badger created but that never reached the disk (the process was killed while it
// wrote it: on Windows the file then holds only zeros) makes badger panic when it opens the
// database, in a goroutine nobody can recover from: the app dies right after the password. Before
// opening, RepairUnwrittenTables finds such tables, keeps a copy of the whole database, and removes
// them from the MANIFEST. What is lost is the content of those tables: the writes of the last
// moments before the process was killed.

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// RepairUnwrittenTables returns the IDs of the tables it removed and where the copy of the
// database before the repair is (empty when nothing had to be repaired).
func RepairUnwrittenTables(dir string) ([]uint64, string, error) {
	broken, err := unwrittenTables(dir)
	if err != nil || len(broken) == 0 {
		return nil, "", err
	}
	backup := dir + ".backup-" + time.Now().Format("20060102-150405")
	if err := copyDir(dir, backup); err != nil {
		return nil, "", fmt.Errorf("keeping a copy before the repair: %w", err)
	}
	levels, err := manifestLevels(filepath.Join(dir, "MANIFEST"))
	if err != nil {
		return nil, backup, err
	}
	set := &pb.ManifestChangeSet{}
	for _, id := range broken {
		lvl, ok := levels[id]
		if !ok {
			continue // not in the MANIFEST: badger ignores the file
		}
		set.Changes = append(set.Changes, &pb.ManifestChange{Id: id, Op: pb.ManifestChange_DELETE, Level: lvl})
	}
	if len(set.Changes) > 0 {
		if err := appendManifest(filepath.Join(dir, "MANIFEST"), set); err != nil {
			return nil, backup, err
		}
	}
	for _, id := range broken {
		name := filepath.Join(dir, fmt.Sprintf("%06d.sst", id))
		if err := os.Rename(name, name+".unwritten"); err != nil {
			return nil, backup, err
		}
	}
	return broken, backup, nil
}

// unwrittenTables lists the .sst files that hold nothing but zeros.
func unwrittenTables(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []uint64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sst") {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSuffix(name, ".sst"), 10, 64)
		if err != nil {
			continue
		}
		zero, err := allZero(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if zero {
			out = append(out, id)
		}
	}
	return out, nil
}

func allZero(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return false, nil
			}
		}
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// manifestLevels replays a badger MANIFEST: table ID -> level, for the tables that exist.
func manifestLevels(path string) (map[uint64]uint32, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 8 || !bytes.Equal(raw[:4], []byte("Bdgr")) {
		return nil, errors.New("not a badger MANIFEST")
	}
	levels := map[uint64]uint32{}
	for off := 8; off+8 <= len(raw); {
		n := int(binary.BigEndian.Uint32(raw[off:]))
		sum := binary.BigEndian.Uint32(raw[off+4:])
		off += 8
		if off+n > len(raw) || crc32.Checksum(raw[off:off+n], castagnoli) != sum {
			break // a torn last entry: badger stops there too
		}
		var cs pb.ManifestChangeSet
		if err := proto.Unmarshal(raw[off:off+n], &cs); err != nil {
			return nil, err
		}
		for _, c := range cs.Changes {
			switch c.Op {
			case pb.ManifestChange_CREATE:
				levels[c.Id] = c.Level
			case pb.ManifestChange_DELETE:
				delete(levels, c.Id)
			}
		}
		off += n
	}
	return levels, nil
}

func appendManifest(path string, set *pb.ManifestChangeSet) error {
	body, err := proto.Marshal(set)
	if err != nil {
		return err
	}
	var head [8]byte
	binary.BigEndian.PutUint32(head[0:4], uint32(len(body)))
	binary.BigEndian.PutUint32(head[4:8], crc32.Checksum(body, castagnoli))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(head[:], body...)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
