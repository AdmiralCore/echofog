package main

import (
	"flag"
	"fmt"

	"github.com/echofog/echofog-core/internal/p2p/dht"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/internal/storage"
)

// cacheParts are what the node learns from the network and learns again after a start: dropping
// them costs a few minutes of looking around, never the identity, the wallet, the people invited
// to a hidden node or anybody's money.
var cacheParts = []struct{ prefix, what string }{
	{dht.RecordPrefix, "DHT records (adverts, key bundles, addresses)"},
	{"saved:peers:", "known peers and their addresses"},
	{"vpnmarket:rating:", "exit ratings"},
	{"bcache:", "cached key bundles of others"},
}

// optional parts, each behind its own flag: they hold something for somebody
var (
	mailPart = struct{ prefix, what string }{messenger.MailboxPrefix, "mail kept for offline people"}
	bansPart = struct{ prefix, what string }{"bans:peer:", "bans of misbehaving peers"}
)

func cleanCmd(args []string) int {
	f := flag.NewFlagSet("clean", flag.ExitOnError)
	var k keyFlags
	k.register(f)
	mail := f.Bool("mail", false, "also drop the mail kept for people who are offline (it is lost for them)")
	bans := f.Bool("bans", false, "also forget the bans of misbehaving peers")
	dry := f.Bool("dry-run", false, "only count what would be dropped")
	f.Usage = func() {
		fmt.Fprintln(f.Output(), `Usage: echofog-node clean [flags]

Drops what the node learned from the network, which it learns again within minutes of the next
start: DHT records, known peers and their addresses, exit ratings, cached key bundles. Use it when
the node holds stale entries (a peer that moved or got a new key). The identity, the TON wallet,
the exit's ledger, the access secret and who joined a hidden node are never touched.

Stop the node first: the database opens in one process only. With Docker, in deploy/:
  docker compose stop node
  docker compose run --rm node clean
  docker compose up -d`)
		f.PrintDefaults()
	}
	_ = f.Parse(args)

	parts := append([]struct{ prefix, what string }{}, cacheParts...)
	if *mail {
		parts = append(parts, mailPart)
	}
	if *bans {
		parts = append(parts, bansPart)
	}
	n, pass, err := k.newNode()
	if err != nil {
		fatal("%v", err)
	}
	if !n.IsRegistered() {
		fatal("no identity in %s: nothing to clean", k.dataDir)
	}
	if err := n.Login([]byte(pass)); err != nil {
		fatal("cannot open the node's data (is the node still running?): %v", err)
	}
	defer n.Storage.Close()

	total := 0
	for _, p := range parts {
		count, err := dropPrefix(n.Storage, p.prefix, *dry)
		if err != nil {
			fatal("%s: %v", p.what, err)
		}
		total += count
		verb := "dropped"
		if *dry {
			verb = "would drop"
		}
		fmt.Printf("%-50s %s %d\n", p.what, verb, count)
	}
	if !*mail {
		fmt.Println("mail kept for offline people: kept (--mail drops it)")
	}
	if *dry {
		fmt.Printf("%d entries would be dropped; nothing changed.\n", total)
		return 0
	}
	fmt.Printf("%d entries dropped. Start the node again: it finds its peers through the bootstrap nodes.\n", total)
	return 0
}

// dropPrefix deletes every key under prefix and returns how many there were.
func dropPrefix(st storage.Storage, prefix string, dry bool) (int, error) {
	count := 0
	for {
		entries, err := st.FindValues([]byte(prefix), 1000)
		if err != nil {
			return count, err
		}
		if len(entries) == 0 {
			return count, nil
		}
		if dry {
			if len(entries) < 1000 {
				return count + len(entries), nil
			}
			// counting a large table without deleting: page through it
			n, err := countPrefix(st, prefix)
			return n, err
		}
		for _, e := range entries {
			if err := st.Delete(e.Key); err != nil {
				return count, err
			}
			count++
		}
	}
}

func countPrefix(st storage.Storage, prefix string) (int, error) {
	count := 0
	var after []byte
	for {
		entries, err := st.FindValuesSeek([]byte(prefix), after, 1000)
		if err != nil {
			return count, err
		}
		for _, e := range entries {
			if string(e.Key) != string(after) {
				count++
			}
		}
		if len(entries) < 1000 {
			return count, nil
		}
		after = entries[len(entries)-1].Key
	}
}
