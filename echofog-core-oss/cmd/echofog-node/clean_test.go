package main

import (
	"testing"

	"github.com/echofog/echofog-core/internal/p2p/dht"
	"github.com/echofog/echofog-core/internal/services/messenger"
)

// clean drops what the network fills in again and keeps the identity and, unless asked, the mail
// kept for offline people.
func TestCleanDropsNetworkDataOnly(t *testing.T) {
	k := keyFlags{dataDir: t.TempDir(), password: "pass123"}
	n, pass, err := k.newNode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Register([]byte(pass), nil); err != nil {
		t.Fatal(err)
	}
	if err := n.Login([]byte(pass)); err != nil {
		t.Fatal(err)
	}
	put := func(key string) {
		if err := n.Storage.Set([]byte(key), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 1500; i++ { // more than one page
		put(dht.RecordPrefix + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('0'+i/676)))
	}
	put("saved:peers:abc")
	put("vpnmarket:rating:abc")
	put("bcache:abc")
	put(messenger.MailboxPrefix + "abc")
	n.Storage.Close()

	if dropped, _ := dropPrefixCount(t, k, false); dropped == 0 {
		t.Fatal("nothing was dropped")
	}
	n2, _, _ := k.newNode()
	if err := n2.Login([]byte(pass)); err != nil {
		t.Fatalf("the identity must survive a clean: %v", err)
	}
	defer n2.Storage.Close()
	for _, p := range []string{dht.RecordPrefix, "saved:peers:", "vpnmarket:rating:", "bcache:"} {
		if e, _ := n2.Storage.FindValues([]byte(p), 1); len(e) > 0 {
			t.Fatalf("%s survived the clean", p)
		}
	}
	if e, _ := n2.Storage.FindValues([]byte(messenger.MailboxPrefix), 1); len(e) != 1 {
		t.Fatal("mail for offline people must be kept without --mail")
	}
}

// dropPrefixCount runs the clean as the command does, against the data directory of k.
func dropPrefixCount(t *testing.T, k keyFlags, mail bool) (int, error) {
	t.Helper()
	n, pass, err := k.newNode()
	if err != nil {
		return 0, err
	}
	if err := n.Login([]byte(pass)); err != nil {
		return 0, err
	}
	defer n.Storage.Close()
	total := 0
	for _, p := range cacheParts {
		c, err := dropPrefix(n.Storage, p.prefix, false)
		if err != nil {
			return total, err
		}
		total += c
	}
	if c, _ := dropPrefix(n.Storage, dht.RecordPrefix, true); c != 0 {
		t.Fatalf("%d DHT records counted after the clean", c)
	}
	return total, nil
}
