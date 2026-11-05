package bootstrap_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
)

// vectorTime is the instant of the spec vectors: 2026-11-03T00:00:00Z.
var vectorTime = time.Unix(1793664000, 0)

func vectorLink(t testing.TB, which string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "protocol", "v2", "vectors", "link.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	sub, _ := doc[which].(map[string]any)
	s, _ := sub["link"].(string)
	if s == "" {
		t.Fatalf("no %s link in the vector", which)
	}
	return s
}

func TestParseContactLinkIsACardEntry(t *testing.T) {
	es, err := bootstrap.Parse("# the lab node\n"+vectorLink(t, "contact")+"\n", vectorTime)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(es) != 1 || es[0].Kind != bootstrap.KindCard || es[0].Card == nil {
		t.Fatalf("entries %+v", es)
	}
}

// An invitation carries an access secret; it must not sit in a list that may be shared.
func TestParseRefusesInvitationLink(t *testing.T) {
	link := vectorLink(t, "invitation")
	_, err := bootstrap.Parse(link, vectorTime)
	if err == nil {
		t.Fatal("an invitation link was accepted")
	}
	if i := strings.Index(link, "&s="); i >= 0 && strings.Contains(err.Error(), link[i+3:]) {
		t.Fatal("the error repeats the access secret")
	}
}

func TestParseRefusesExpiredLink(t *testing.T) {
	if _, err := bootstrap.Parse(vectorLink(t, "contact"), vectorTime.Add(10*365*24*time.Hour)); err == nil {
		t.Fatal("an expired link was accepted")
	}
}

// FuzzParse: a list comes from a file or a flag. Parse must not panic and must stay bounded.
func FuzzParse(f *testing.F) {
	f.Add("noise3://192.0.2.10:4430?mask=" + hexM + "&device=" + hexA)
	f.Add("reality2-tcp://[2001:db8::1]:443?mask=" + hexM + "&sni=www.example.org")
	f.Add(vectorLink(f, "contact"))
	f.Add(vectorLink(f, "invitation"))
	f.Add("# c\n\n\r\nnoise3://:1?mask=\n")
	f.Fuzz(func(t *testing.T, s string) {
		es, err := bootstrap.Parse(s, vectorTime)
		if err == nil && len(es) > bootstrap.MaxEntries {
			t.Fatalf("%d entries", len(es))
		}
		for _, e := range es {
			if e.Kind == bootstrap.KindCard && e.Card == nil {
				t.Fatal("card entry without a card")
			}
			if e.Kind != bootstrap.KindCard && (e.Addr == nil || e.Addr.GetPort() == 0 || e.Addr.GetPort() > 65535) {
				t.Fatalf("bad address %v", e.Addr)
			}
		}
	})
}
