package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/echofog/echofog-core/internal/v2/installkit"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

// The install-script side of a release (T12.4e):
//
//	efv2-release install-stamp       --template install.sh --keys PUB1,PUB2,PUB3 --key-epoch N --floor X.Y.Z --out install.sh
//	efv2-release install-fingerprint --keys PUB1,PUB2,PUB3 --key-epoch N
//	efv2-release install-record      --manifest manifest.bin --key K1.key --key K2.key --key-epoch N --out-dir DIR
//	efv2-release install-rotation    --new-keys PUB1,PUB2,PUB3 --new-epoch N --key K1.key --key K2.key --out-dir DIR

func parseKeyList(list string) ([]keys.AKey, error) {
	var set []keys.AKey
	for _, k := range strings.Split(list, ",") {
		key, err := keys.ParseText(strings.TrimSpace(k))
		if err != nil {
			return nil, fmt.Errorf("--keys: %w", err)
		}
		set = append(set, key)
	}
	return set, nil
}

func loadSigners(paths multi) ([]*keys.Signer, error) {
	var out []*keys.Signer
	seen := map[string]bool{}
	for _, p := range paths {
		s, err := loadSigner(p)
		if err != nil {
			return nil, err
		}
		if seen[s.Public().Text()] {
			return nil, errors.New("the same --key is given twice: one key is one signature")
		}
		seen[s.Public().Text()] = true
		out = append(out, s)
	}
	if len(out) < installkit.Threshold {
		return nil, fmt.Errorf("at least %d different --key files are needed (the scripts accept %d of the release keys)", installkit.Threshold, installkit.Threshold)
	}
	return out, nil
}

func writeSigned(dir, name string, body string, ns string, signers []*keys.Signer) error {
	sigs, err := installkit.SignRecord([]byte(body), ns, signers...)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".sigs"), sigs, 0o644)
}

func epochFlag(fs *flag.FlagSet, name string) *uint {
	return fs.Uint(name, 0, "key epoch, 1 for the first key set")
}

// epoch32 keeps an epoch inside a sane range before it is narrowed.
func epoch32(u uint) (uint32, error) {
	if u == 0 || u > 1<<30 {
		return 0, errors.New("the key epoch must be between 1 and 2^30")
	}
	return uint32(u), nil
}

func cmdInstallFingerprint(args []string, stdout io.Writer) error {
	fs := newFlags("install-fingerprint")
	keyList := fs.String("keys", "", "comma-separated release public keys (fingerprint of the key block)")
	epoch := epochFlag(fs, "key-epoch")
	script := fs.String("script", "", "an install.sh: print the SHA-256 of the whole file instead")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if given(fs)["script"] {
		if given(fs)["keys"] || given(fs)["key-epoch"] {
			return errors.New("--script is given alone")
		}
		raw, err := os.ReadFile(*script)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, installkit.FileFingerprint(string(raw)))
		return err
	}
	if err := need(fs, "keys", "key-epoch"); err != nil {
		return err
	}
	set, err := parseKeyList(*keyList)
	if err != nil {
		return err
	}
	ep, err := epoch32(*epoch)
	if err != nil {
		return err
	}
	fp, err := installkit.Fingerprint(set, ep)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, fp)
	return err
}

func cmdInstallStamp(args []string, stdout io.Writer) error {
	fs := newFlags("install-stamp")
	tpl := fs.String("template", "", "the install.sh template (its key block says PLACEHOLDER)")
	keyList := fs.String("keys", "", "comma-separated release public keys")
	epoch := epochFlag(fs, "key-epoch")
	floor := fs.String("floor", "", "lowest version the script installs, X.Y.Z")
	out := fs.String("out", "", "the stamped script")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "template", "keys", "key-epoch", "floor", "out"); err != nil {
		return err
	}
	set, err := parseKeyList(*keyList)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*tpl)
	if err != nil {
		return err
	}
	ep, err := epoch32(*epoch)
	if err != nil {
		return err
	}
	stamped, err := installkit.Stamp(string(raw), set, ep, *floor)
	if err != nil {
		return err
	}
	fp, err := installkit.Fingerprint(set, ep)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(stamped), 0o755); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "stamped %s\nfile %s\nkeys %s\nPublish the file value on the site and in the README before the announcement: users compare sha256sum install.sh. The keys value belongs to the key rotation record.\n", *out, installkit.FileFingerprint(stamped), fp)
	return err
}

func cmdInstallRecord(args []string, stdout io.Writer) error {
	fs := newFlags("install-record")
	manifest := fs.String("manifest", "", "manifest file (from assemble)")
	epoch := epochFlag(fs, "key-epoch")
	dir := fs.String("out-dir", "", "folder for release.txt and release.txt.sigs")
	var keyFiles multi
	fs.Var(&keyFiles, "key", "private key file of a release key (give at least two different ones)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "manifest", "key-epoch", "out-dir"); err != nil {
		return err
	}
	_, rel, err := readManifest(*manifest)
	if err != nil {
		return err
	}
	signers, err := loadSigners(keyFiles)
	if err != nil {
		return err
	}
	ep, err := epoch32(*epoch)
	if err != nil {
		return err
	}
	rec, err := installkit.Record(rel, ep)
	if err != nil {
		return err
	}
	if err := writeSigned(*dir, "release.txt", rec, installkit.NSRelease, signers); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "release.txt for %s signed by %d keys in %s\n", rel.GetVersion(), len(signers), *dir)
	return err
}

func cmdInstallRotation(args []string, stdout io.Writer) error {
	fs := newFlags("install-rotation")
	keyList := fs.String("new-keys", "", "comma-separated public keys of the NEW release key set")
	epoch := epochFlag(fs, "new-epoch")
	dir := fs.String("out-dir", "", "folder for rotation.txt and rotation.txt.sigs")
	var keyFiles multi
	fs.Var(&keyFiles, "key", "private key file of an OLD release key (give at least two different ones)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "new-keys", "new-epoch", "out-dir"); err != nil {
		return err
	}
	set, err := parseKeyList(*keyList)
	if err != nil {
		return fmt.Errorf("--new-keys: %w", err)
	}
	signers, err := loadSigners(keyFiles)
	if err != nil {
		return err
	}
	ep, err := epoch32(*epoch)
	if err != nil {
		return err
	}
	rec, err := installkit.Rotation(set, ep)
	if err != nil {
		return err
	}
	if err := writeSigned(*dir, "rotation.txt", rec, installkit.NSRotation, signers); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "rotation.txt to epoch %d signed by %d old keys in %s\n", *epoch, len(signers), *dir)
	return err
}

func given(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}
