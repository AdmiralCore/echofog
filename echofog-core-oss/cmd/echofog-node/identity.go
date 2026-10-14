package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/pkg/node"
	"golang.org/x/term"
)

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// keyFlags are the flags every command that opens the identity takes.
type keyFlags struct {
	dataDir      string
	password     string
	passwordFile string
}

func (k *keyFlags) register(f *flag.FlagSet) {
	f.StringVar(&k.dataDir, "data-dir", getEnv("ECHOFOG_DATA_DIR", "/data"), "where the database and identity live")
	f.StringVar(&k.password, "password", getEnv("ECHOFOG_PASSWORD", ""), "password that encrypts the identity (prefer --password-file; without both one is generated and kept in the data directory)")
	f.StringVar(&k.passwordFile, "password-file", getEnv("ECHOFOG_PASSWORD_FILE", ""), "file holding the password (for example a Docker secret)")
}

// nodePassword returns the identity password: given, from a file, or one generated on first run and
// kept in the data directory so the node can restart unattended.
func (k keyFlags) nodePassword() (string, error) {
	if k.password != "" {
		return k.password, nil
	}
	if k.passwordFile != "" {
		data, err := os.ReadFile(k.passwordFile)
		if err != nil {
			return "", fmt.Errorf("reading --password-file: %w", err)
		}
		if p := strings.TrimRight(string(data), "\r\n"); p != "" {
			return p, nil
		}
		return "", errors.New("--password-file is empty")
	}
	file := filepath.Join(k.dataDir, ".node_password")
	if data, err := os.ReadFile(file); err == nil && len(data) > 0 {
		return string(data), nil
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	pass := hex.EncodeToString(random)
	if err := os.WriteFile(file, []byte(pass), 0o600); err != nil {
		return "", err
	}
	fmt.Printf("Generated the identity password and saved it to %s\n", file)
	return pass, nil
}

func (k keyFlags) newNode() (*node.Node, string, error) {
	if err := os.MkdirAll(k.dataDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("cannot create the data directory: %w", err)
	}
	pass, err := k.nodePassword()
	if err != nil {
		return nil, "", err
	}
	cfg := config.DefaultConfig("")
	cfg.Storage.DatabasePath = filepath.Join(k.dataDir, "db")
	return node.NewNode(context.Background(), cfg, "echofog-node"), pass, nil
}

// readMnemonic takes the recovery phrase from --mnemonic, --mnemonic-file, ECHOFOG_MNEMONIC, or
// asks for it (hidden) when there is a terminal. The words are checked against the BIP-39 list,
// so a typo shows up here and not as a different identity.
func readMnemonic(inline, file string) (string, error) {
	phrase := inline
	if phrase == "" && file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading --mnemonic-file: %w", err)
		}
		phrase = string(data)
	}
	if phrase == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			// piped in: echo "word word ..." | echofog-node restore
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil && line == "" {
				return "", errors.New("no recovery phrase: pass --mnemonic-file, set ECHOFOG_MNEMONIC, or type it at the prompt")
			}
			phrase = line
		} else {
			fmt.Print("Recovery phrase (the words are not shown): ")
			raw, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				return "", err
			}
			phrase = string(raw)
		}
	}
	return keymanager.CheckPhrase(phrase)
}

func printPhrase(phrase string) {
	fmt.Println()
	fmt.Println("==================================================================")
	fmt.Println(" WRITE DOWN THIS RECOVERY PHRASE AND KEEP IT OFFLINE.")
	fmt.Println(" It restores this node's identity and its TON wallet. It is shown once.")
	fmt.Println("==================================================================")
	for i, w := range strings.Fields(phrase) {
		fmt.Printf(" %2d. %-12s", i+1, w)
		if (i+1)%4 == 0 {
			fmt.Println()
		}
	}
	fmt.Println("==================================================================")
	fmt.Println()
}

func initCmd(args []string) int {
	f := flag.NewFlagSet("init", flag.ExitOnError)
	var k keyFlags
	k.register(f)
	f.Usage = func() {
		fmt.Fprintln(f.Output(), "Usage: echofog-node init [flags]\n\nCreates a new identity in the data directory and prints its recovery phrase.")
		f.PrintDefaults()
	}
	_ = f.Parse(args)

	n, pass, err := k.newNode()
	if err != nil {
		fatal("%v", err)
	}
	if n.IsRegistered() {
		fatal("an identity already exists in %s: start it with \"echofog-node run\", or use another --data-dir", k.dataDir)
	}
	phrase, err := n.Register([]byte(pass), nil)
	if err != nil {
		fatal("cannot create the identity: %v", err)
	}
	printPhrase(phrase)
	fmt.Println("Identity created. Start the node with: echofog-node run")
	return 0
}

func restoreCmd(args []string) int {
	f := flag.NewFlagSet("restore", flag.ExitOnError)
	var k keyFlags
	k.register(f)
	mnemonic := f.String("mnemonic", getEnv("ECHOFOG_MNEMONIC", ""), "the recovery phrase (visible in the process list: prefer --mnemonic-file or the prompt)")
	mnemonicFile := f.String("mnemonic-file", getEnv("ECHOFOG_MNEMONIC_FILE", ""), "file holding the recovery phrase")
	passphrase := f.String("passphrase", getEnv("ECHOFOG_MNEMONIC_PASSPHRASE", ""), "the optional extra passphrase, if the phrase was made with one")
	force := f.Bool("force", false, "replace an identity that already exists in the data directory")
	f.Usage = func() {
		fmt.Fprintln(f.Output(), "Usage: echofog-node restore [flags]\n\nRestores an identity (and its TON wallet) from its recovery phrase. Without --mnemonic\nor --mnemonic-file the phrase is asked for, hidden, or read from standard input.")
		f.PrintDefaults()
	}
	_ = f.Parse(args)

	phrase, err := readMnemonic(*mnemonic, *mnemonicFile)
	if err != nil {
		fatal("%v", err)
	}
	n, pass, err := k.newNode()
	if err != nil {
		fatal("%v", err)
	}
	if n.IsRegistered() {
		if !*force {
			fatal("an identity already exists in %s; add --force to replace it (its messages and contacts are deleted)", k.dataDir)
		}
		if err := os.RemoveAll(filepath.Join(k.dataDir, "db")); err != nil {
			fatal("cannot remove the old identity: %v", err)
		}
	}
	var pp []byte
	if *passphrase != "" {
		pp = []byte(*passphrase)
	}
	if err := n.Recover(phrase, pp, []byte(pass)); err != nil {
		fatal("cannot restore the identity: %v", err)
	}
	fmt.Println("Identity restored. Start the node with: echofog-node run")
	return 0
}
