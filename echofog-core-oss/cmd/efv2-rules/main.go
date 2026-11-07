// Command efv2-rules is the offline tool of the rules curator (the owner, H3a): it makes the rules
// key, signs the country rules file and checks one. The key is separate from the release keys and
// lives offline; the tool refuses to write it inside a git work tree.
//
//	efv2-rules keygen --out BASE                         BASE.key (private seed, hex, 0600), BASE.pub
//	efv2-rules sign   --key K --body body.json --out rules.bin [--prev old.bin] [--seq N]
//	                  [--valid-days 30] [--now UNIX]
//	efv2-rules verify --pub PUBHEX --in rules.bin [--min-seq N] [--now UNIX]
//
// The tool sets seq and valid_until itself: seq is the previous file's plus one (1 for the first
// file) and never goes down; valid_until is 30 days from now (at most 45). The file is whole, one
// signed object; there is no telemetry and no partial update.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "efv2-rules:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("a command is required: keygen, sign or verify")
	}
	switch args[0] {
	case "keygen":
		return cmdKeygen(args[1:], stdout)
	case "sign":
		return cmdSign(args[1:], stdout)
	case "verify":
		return cmdVerify(args[1:], stdout)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func given(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

func need(fs *flag.FlagSet, names ...string) error {
	set := given(fs)
	for _, n := range names {
		if !set[n] {
			return fmt.Errorf("--%s is required", n)
		}
	}
	return nil
}

func clock(sec int64, set bool) func() time.Time {
	if set {
		return func() time.Time { return time.Unix(sec, 0) }
	}
	return time.Now
}

// ---- keygen

func cmdKeygen(args []string, stdout io.Writer) error {
	fs := newFlags("keygen")
	out := fs.String("out", "", "base path: writes BASE.key (private) and BASE.pub")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "out"); err != nil {
		return err
	}
	abs, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if dir := inGitWorkTree(filepath.Dir(abs)); dir != "" {
		return fmt.Errorf("refusing to write a private key inside the git work tree %s: use offline media", dir)
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	f, err := os.OpenFile(abs+".key", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(hex.EncodeToString(seed) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	for i := range seed {
		seed[i] = 0
	}
	if werr != nil {
		_ = os.Remove(abs + ".key")
		return werr
	}
	if err := os.WriteFile(abs+".pub", []byte(pub+"\n"), 0o644); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, pub)
	return err
}

func inGitWorkTree(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	if fi, err := os.Stat(path); err == nil && os.PathSeparator == '/' && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("key file %s is readable by others (mode %v): chmod 600", filepath.Base(path), fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("key file is not a hex seed")
	}
	defer func() {
		for i := range seed {
			seed[i] = 0
		}
	}()
	return ed25519.NewKeyFromSeed(seed), nil
}

func readLimited(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), max)
	}
	return b, nil
}

// ---- sign

const (
	defaultDays = 30
	maxDays     = 45
)

func cmdSign(args []string, stdout io.Writer) error {
	fs := newFlags("sign")
	keyPath := fs.String("key", "", "rules key (hex seed file)")
	bodyPath := fs.String("body", "", "JSON body: version, allowed_cidrs, neighbors, ...")
	outPath := fs.String("out", "", "the signed rules file")
	prevPath := fs.String("prev", "", "the previous signed file (the new seq is its seq + 1)")
	seqFlag := fs.Uint64("seq", 0, "sequence number (default: previous + 1, or 1)")
	days := fs.String("valid-days", strconv.Itoa(defaultDays), "validity in days, 1 to 45")
	nowFlag := fs.Int64("now", 0, "time in unix seconds (tests)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "key", "body", "out"); err != nil {
		return err
	}
	set := given(fs)
	validDays, err := strconv.Atoi(*days)
	if err != nil || validDays < 1 || validDays > maxDays {
		return fmt.Errorf("--valid-days must be a number from 1 to %d", maxDays)
	}
	priv, err := loadKey(*keyPath)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	now := clock(*nowFlag, set["now"])

	var prevSeq uint64
	if *prevPath != "" {
		raw, err := readLimited(*prevPath, rp.MaxRulesFile)
		if err != nil {
			return err
		}
		prev, err := rp.ParseAt(raw, pub, 0, now)
		if err != nil {
			return fmt.Errorf("--prev is not a file of this key: %w", err)
		}
		prevSeq = prev.Seq
	}
	seq := prevSeq + 1
	if set["seq"] {
		if *seqFlag == 0 || *seqFlag <= prevSeq {
			return fmt.Errorf("--seq must be above the previous seq (%d)", prevSeq)
		}
		seq = *seqFlag
	}

	raw, err := readLimited(*bodyPath, rp.MaxRulesFile)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	body["seq"] = seq
	body["valid_until"] = now().Add(time.Duration(validDays) * 24 * time.Hour).Unix()
	enc, err := json.Marshal(body)
	if err != nil {
		return err
	}
	file := rp.Sign(priv, enc)
	// What is written is what the node will accept.
	rules, err := rp.ParseAt(file, pub, prevSeq, now)
	if err != nil {
		return fmt.Errorf("the body is refused by the node's own parser: %w", err)
	}
	tmp := *outPath + ".tmp"
	if err := os.WriteFile(tmp, file, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, *outPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_, err = fmt.Fprintf(stdout, "signed seq %d, valid until %s (%d days)\n", rules.Seq, rules.ValidUntil().UTC().Format(time.RFC3339), validDays)
	return err
}

// ---- verify

func cmdVerify(args []string, stdout io.Writer) error {
	fs := newFlags("verify")
	pubHex := fs.String("pub", "", "rules public key (hex)")
	in := fs.String("in", "", "the signed rules file")
	minSeq := fs.Uint64("min-seq", 0, "refuse a seq below this")
	nowFlag := fs.Int64("now", 0, "time in unix seconds (tests)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "pub", "in"); err != nil {
		return err
	}
	pub, err := hex.DecodeString(strings.TrimSpace(*pubHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("--pub is not a 64-character hex key")
	}
	raw, err := readLimited(*in, rp.MaxRulesFile)
	if err != nil {
		return err
	}
	now := clock(*nowFlag, given(fs)["now"])
	rules, err := rp.ParseAt(raw, ed25519.PublicKey(pub), *minSeq, now)
	if err != nil {
		return err
	}
	if rules.Expired() {
		// Printed for the curator, and a nonzero exit for the scripts: a monthly update is overdue.
		fmt.Fprintf(stdout, "seq %d, EXPIRED on %s: the signature is good; verified entries no longer count and the restrictions of the file stay\n",
			rules.Seq, rules.ValidUntil().UTC().Format(time.RFC3339))
		return errors.New("the rules file has expired")
	}
	left := int(rules.ValidUntil().Sub(now()).Hours() / 24)
	_, err = fmt.Fprintf(stdout, "seq %d, valid until %s (%d days left)\n", rules.Seq, rules.ValidUntil().UTC().Format(time.RFC3339), left)
	return err
}
