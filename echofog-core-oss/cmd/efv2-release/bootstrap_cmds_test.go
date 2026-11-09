package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// TestBootstrapSignVerify is the owner's flow for the official entry list (T5.6): sign the text
// list with a release key, verify it, and refuse a changed file or another key.
func TestBootstrapSignVerify(t *testing.T) {
	dir := t.TempDir()
	priv, pub := keygen(t, dir, "release")
	list := filepath.Join(dir, "list.txt")
	line := "noise3://192.0.2.10:4430?mask=0202020202020202020202020202020202020202020202020202020202020202&device=0101010101010101010101010101010101010101010101010101010101010101"
	if err := os.WriteFile(list, []byte("# official entry nodes\n"+line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(30 * 24 * time.Hour).Unix()
	signed := filepath.Join(dir, "list.bin")
	out := mustDo(t, "bootstrap-sign", "--list", list, "--key", priv, "--seq", "7",
		"--not-after", itoa(notAfter), "--out", signed)
	if !strings.Contains(out, "seq 7") || !strings.Contains(out, "1 entries") {
		t.Fatalf("sign printed %q", out)
	}
	out = mustDo(t, "bootstrap-verify", "--keys", pub, signed)
	if !strings.Contains(out, "seq 7") || !strings.Contains(out, "1 entries") {
		t.Fatalf("verify printed %q", out)
	}
	// Expired for a later clock.
	if _, err := do(t, "bootstrap-verify", "--keys", pub, "--now", itoa(time.Now().Add(31*24*time.Hour).Unix()), signed); err == nil {
		t.Fatal("an expired list verified")
	}
	// A changed byte and another key are refused.
	raw, err := os.ReadFile(signed)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff
	bad := filepath.Join(dir, "bad.bin")
	if err := os.WriteFile(bad, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := do(t, "bootstrap-verify", "--keys", pub, bad); err == nil {
		t.Fatal("a forged list verified")
	}
	_, other := keygen(t, dir, "other")
	if _, err := do(t, "bootstrap-verify", "--keys", other, signed); err == nil {
		t.Fatal("a foreign key verified the list")
	}
	// A bad line is not signed at all.
	badList := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(badList, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := do(t, "bootstrap-sign", "--list", badList, "--key", priv, "--seq", "8",
		"--not-after", itoa(notAfter), "--out", filepath.Join(dir, "x.bin")); err == nil {
		t.Fatal("a list with a bad line was signed")
	}
}

// TestAssembleCarriesSignedList: the owner builds the manifest with the signed entry list in it
// (T5.6 point 2), so the update flow delivers the list together with the manifest.
func TestAssembleCarriesSignedList(t *testing.T) {
	dir := t.TempDir()
	priv, pub := keygen(t, dir, "release")
	_, pub2 := keygen(t, dir, "cosigner")
	line := "noise3://192.0.2.10:4430?mask=0202020202020202020202020202020202020202020202020202020202020202&device=0101010101010101010101010101010101010101010101010101010101010101"
	list := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(list, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	signed := filepath.Join(dir, "list.bin")
	mustDo(t, "bootstrap-sign", "--list", list, "--key", priv, "--seq", "7",
		"--not-after", itoa(time.Now().Add(30*24*time.Hour).Unix()), "--out", signed)
	at := time.Now().Unix()
	manifest := filepath.Join(dir, "manifest.bin")
	mustDo(t, "assemble", "--version", "0.3.1", "--released-at", itoa(at),
		"--bootstrap-list", signed, "--out", manifest)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	rel := &pb.Release{}
	if err := proto.Unmarshal(raw, rel); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(signed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rel.GetBootstrapList(), want) {
		t.Fatalf("the manifest carries %d list bytes, want %d", len(rel.GetBootstrapList()), len(want))
	}
	cosig := filepath.Join(dir, "K2.cosig")
	mustDo(t, "cosign", "--manifest", manifest, "--key", filepath.Join(dir, "cosigner.key"), "--out", cosig)
	sealed := filepath.Join(dir, "release.bin")
	mustDo(t, "sign", "--manifest", manifest, "--key", priv, "--cosig", cosig,
		"--not-after", itoa(at+30*86400), "--out", sealed)
	out := mustDo(t, "verify", "--keys", pub+","+pub2, sealed)
	if !strings.Contains(out, "0.3.1") {
		t.Fatalf("verify printed %q", out)
	}
}
