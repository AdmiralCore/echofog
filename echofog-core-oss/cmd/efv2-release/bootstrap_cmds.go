package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

// cmdBootstrapSign signs the official entry list of a release (T5.6). The output is what the
// release build puts at internal/v2/bootstrap/release/list.bin before `go build`, or what the
// update flow drops next to node.json as bootstrap-update.bin.
func cmdBootstrapSign(args []string, stdout io.Writer) error {
	fs := newFlags("bootstrap-sign")
	list := fs.String("list", "", "text list of T5.5 entries")
	key := fs.String("key", "", "private key file of a release key")
	seq := fs.Uint64("seq", 0, "list sequence, newer than the applied one")
	notAfter := fs.Uint64("not-after", 0, "term of the list, unix seconds")
	out := fs.String("out", "", "signed list output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "list", "key", "seq", "not-after", "out"); err != nil {
		return err
	}
	info, err := os.Stat(*list)
	if err != nil {
		return err
	}
	if info.Size() > bootstrap.ListMaxBytes {
		return fmt.Errorf("the list is larger than %d bytes", bootstrap.ListMaxBytes)
	}
	text, err := os.ReadFile(*list)
	if err != nil {
		return err
	}
	entries, err := bootstrap.Parse(string(text), time.Now())
	if err != nil {
		return fmt.Errorf("the list is not usable: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("the list has no entries")
	}
	s, err := loadSigner(*key)
	if err != nil {
		return err
	}
	raw, err := bootstrap.SealList(s, *seq, *notAfter, string(text))
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, raw, 0o644); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "signed list seq %d, %d entries, until %d, by %s\n",
		*seq, len(entries), *notAfter, s.Public().Text())
	return err
}

// cmdBootstrapVerify checks a signed list against the release public key set.
func cmdBootstrapVerify(args []string, stdout io.Writer) error {
	fs := newFlags("bootstrap-verify")
	keyList := fs.String("keys", "", "comma-separated release public keys")
	nowUnix := fs.Int64("now", 0, "verification time, unix seconds (default: now)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "keys"); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: efv2-release bootstrap-verify --keys PUB1,PUB2,PUB3 list.bin")
	}
	var set []keys.AKey
	for _, k := range strings.Split(*keyList, ",") {
		key, err := keys.ParseText(strings.TrimSpace(k))
		if err != nil {
			return fmt.Errorf("--keys: %w", err)
		}
		set = append(set, key)
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	now := time.Now()
	if *nowUnix != 0 {
		now = time.Unix(*nowUnix, 0)
	}
	l, err := bootstrap.OpenList(raw, set, now)
	if err != nil {
		return fmt.Errorf("NOT VALID: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "valid: seq %d, %d entries, until %d, signer %s\n",
		l.Seq, len(l.Entries), l.NotAfter, l.Signer.Text())
	return err
}
