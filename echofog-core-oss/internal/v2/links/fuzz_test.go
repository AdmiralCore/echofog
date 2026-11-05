package links

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FuzzOpen: a link comes from anywhere. Open must not panic, must stay bounded (decompression
// bombs, huge parameters) and must never put the access secret into the preview.
func FuzzOpen(f *testing.F) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "protocol", "v2", "vectors", "link.json"))
	if err != nil {
		f.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		f.Fatal(err)
	}
	for _, k := range []string{"contact", "invitation"} {
		sub, _ := doc[k].(map[string]any)
		if s, _ := sub["link"].(string); s != "" {
			f.Add(s)
			f.Add(strings.Replace(s, "echofog://join?", "echofog://join/?", 1))
			f.Add("https://join.example.org/join/#" + strings.TrimPrefix(s, "echofog://"))
		}
	}
	var z bytes.Buffer
	w, _ := flate.NewWriter(&z, flate.BestCompression)
	_, _ = w.Write(make([]byte, 4<<20))
	_ = w.Close()
	f.Add("echofog://join?v=2&card=" + base64.RawURLEncoding.EncodeToString(append([]byte{1}, z.Bytes()...)))
	f.Add("echofog://join?v=2&card=AAAA&s=" + strings.Repeat("A", 22))
	f.Add("  echofog://join/?v=2&v=2  ")
	f.Add("")

	now := time.Unix(1793664000, 0)
	f.Fuzz(func(t *testing.T, s string) {
		p, err := Open(s, now)
		if err != nil {
			if p != nil {
				t.Fatal("preview with an error")
			}
			if UserMessage(err) == "" {
				t.Fatal("no user message")
			}
			return
		}
		if got := fmt.Sprintf("%+v", *p); secretIndex(s) >= 0 && strings.Contains(got, s[secretIndex(s)+3:]) && len(s[secretIndex(s)+3:]) >= 22 {
			t.Fatal("preview carries the access secret")
		}
	})
}

// secretIndex is the index of the secret parameter of a link, or -1.
func secretIndex(s string) int { return strings.Index(s, "&s=") }
