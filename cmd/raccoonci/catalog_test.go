package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Raccoon-CI/raccoonci/pkg/catalog"
)

func TestCatalogKeygenAndSign(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "feed.key")
	if err := catalogKeygen([]string{"-out", keyFile}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(keyFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("private key file: %v %v", fi, err)
	}
	// An existing file is never overwritten.
	if err := catalogKeygen([]string{"-out", keyFile}); err == nil {
		t.Error("keygen overwrote an existing key")
	}
	if err := catalogKeygen(nil); err == nil {
		t.Error("keygen without -out")
	}

	feed := filepath.Join(dir, "catalog.yaml")
	body := "toolchains:\n  zig: {title: Zig, commands: [zig]}\n"
	os.WriteFile(feed, []byte(body), 0o644)

	// The key is read from the environment only.
	t.Setenv(signingKeyEnv, "")
	if err := catalogSign([]string{feed}); err == nil {
		t.Error("sign without a key in the environment")
	}
	priv, _ := os.ReadFile(keyFile)
	t.Setenv(signingKeyEnv, strings.TrimSpace(string(priv)))
	if err := catalogSign([]string{feed}); err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(feed + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := catalog.ParsePrivateKey(string(priv))
	if strings.TrimSpace(string(sig)) != catalog.Sign(key, []byte(body)) {
		t.Error("the .sig does not hold the signature of the file")
	}

	// A feed that would not be accepted is not signed.
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte("toolchains: [not a map"), 0o644)
	if err := catalogSign([]string{bad}); err == nil {
		t.Error("signed an invalid catalog")
	}
	if _, err := os.Stat(bad + ".sig"); err == nil {
		t.Error("a .sig was written for an invalid catalog")
	}
}

func TestCatalogFlagsSeveralKeys(t *testing.T) {
	a, _, _ := catalog.GenerateKey()
	b, _, _ := catalog.GenerateKey()
	src, err := catalogFlags{url: "https://x.test/c.yaml", key: a + "," + b}.source()
	if err != nil || len(src.PublicKeys) != 2 {
		t.Fatalf("%v %v", src, err)
	}
	// Nothing named: the built-in feed address and keys, if there are any.
	src, err = (catalogFlags{}).source()
	if err != nil {
		t.Fatal(err)
	}
	if (catalog.DefaultFeedURL == "") != (src == nil) || (src != nil && src.URL != catalog.DefaultFeedURL) {
		t.Errorf("nothing named: want the default feed %q, got %v", catalog.DefaultFeedURL, src)
	}
	if _, err = (catalogFlags{url: "https://x.test/c.yaml", key: "junk"}).source(); err == nil {
		t.Error("a bad key must be an error")
	}
}
