package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// FuzzParseSchema keeps the store header parser total: any bytes are either the number plus the
// magic, or ErrSchemaCorrupt.
func FuzzParseSchema(f *testing.F) {
	good := make([]byte, 4+len(schemaMagic))
	binary.BigEndian.PutUint32(good, Schema)
	copy(good[4:], schemaMagic)
	f.Add(good)
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 1})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add(append([]byte{0, 0, 0, 200}, []byte("EchoFog")...))

	f.Fuzz(func(t *testing.T, raw []byte) {
		version, err := parseSchema(raw)
		if err != nil {
			if !errors.Is(err, ErrSchemaCorrupt) {
				t.Fatalf("parseSchema(%q) = %v, want ErrSchemaCorrupt", raw, err)
			}
			return
		}
		if len(raw) < 4+len(schemaMagic) {
			t.Fatalf("parseSchema accepted %d bytes", len(raw))
		}
		if !bytes.Equal(raw[4:4+len(schemaMagic)], schemaMagic) {
			t.Fatalf("parseSchema accepted a wrong magic: %q", raw[4:])
		}
		if want := binary.BigEndian.Uint32(raw[:4]); version != want {
			t.Fatalf("parseSchema = %d, want %d", version, want)
		}
	})
}
