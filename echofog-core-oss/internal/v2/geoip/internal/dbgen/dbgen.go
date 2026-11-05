// Package dbgen turns a DB-IP Lite Country CSV into the compact database embedded by package
// geoip. It is a build-time tool, not part of the node: the go:generate entry point is the
// build-ignored gen.go in the parent package.
package dbgen

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/echofog/echofog-core/internal/v2/geoip/internal/codec"
)

// Stats describes one generation.
type Stats struct {
	Ranges    int
	IPv4      int
	IPv6      int
	Skipped   int
	Countries int
	Bytes     int
}

// Run reads the CSV (plain or gzip) and writes the database to outPath.
func Run(inPath, outPath string) (Stats, error) {
	var st Stats
	f, err := os.Open(inPath)
	if err != nil {
		return st, err
	}
	defer f.Close()

	r, err := openCSV(f)
	if err != nil {
		return st, err
	}

	records, skipped, err := readRecords(r)
	if err != nil {
		return st, err
	}
	if len(records) == 0 {
		return st, errors.New("no usable ranges")
	}

	var buf bytes.Buffer
	if err := codec.Encode(&buf, records); err != nil {
		return st, err
	}
	data := buf.Bytes()

	if err := replaceFile(outPath, data); err != nil {
		return st, err
	}

	st.Ranges = len(records)
	st.Bytes = len(data)
	st.Skipped = skipped
	st.Countries = countCountries(records)
	for _, rec := range records {
		if rec.Start.Is4() {
			st.IPv4++
		} else {
			st.IPv6++
		}
	}
	return st, nil
}

func openCSV(f *os.File) (*csv.Reader, error) {
	br := bufio.NewReaderSize(f, 1<<20)
	head, err := br.Peek(2)
	if err != nil && err != io.EOF {
		return nil, err
	}
	var src io.Reader = br
	if len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		src = gz
	}
	cr := csv.NewReader(src)
	cr.FieldsPerRecord = 3
	cr.ReuseRecord = true
	return cr, nil
}

func readRecords(cr *csv.Reader) ([]codec.Record, int, error) {
	var records []codec.Record
	skipped := 0
	line := 0
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, skipped, fmt.Errorf("line %d: %w", line+1, err)
		}
		line++
		start, err := netip.ParseAddr(row[0])
		if err != nil {
			return nil, skipped, fmt.Errorf("line %d: start: %w", line, err)
		}
		end, err := netip.ParseAddr(row[1])
		if err != nil {
			return nil, skipped, fmt.Errorf("line %d: end: %w", line, err)
		}
		if start.Is4() != end.Is4() || end.Less(start) {
			return nil, skipped, fmt.Errorf("line %d: invalid range %s-%s", line, start, end)
		}
		code := row[2]
		if code == "ZZ" {
			skipped++
			continue
		}
		records = append(records, codec.Record{Start: start, End: end, Country: code})
	}
	return records, skipped, nil
}

func countCountries(records []codec.Record) int {
	seen := make(map[string]struct{})
	for _, rec := range records {
		seen[rec.Country] = struct{}{}
	}
	return len(seen)
}

// replaceFile writes data to a temporary file beside outPath and renames it into place, so an
// interrupted generation never leaves a half-written database behind.
func replaceFile(outPath string, data []byte) error {
	dir := filepath.Dir(outPath)
	tmp, err := os.CreateTemp(dir, ".country-*.bin")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, outPath)
}
