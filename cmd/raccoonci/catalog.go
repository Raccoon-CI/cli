package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Raccoon-CI/lib/pkg/catalog"
)

// catalogFlags say where the catalog comes from, in layers: the built-in
// one, then the one pulled from URL (cached on disk), then the local file.
type catalogFlags struct {
	file string
	url  string
	key  string
}

func (f catalogFlags) source() (*catalog.Source, error) {
	url, keys, err := catalog.ResolveFeed(f.url, f.key)
	if err != nil {
		return nil, fmt.Errorf("-catalog-key: %w", err)
	}
	if url == "" {
		return nil, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	// One cache per feed and key set, so switching feeds can't mix their copies.
	var ks []string
	for _, k := range keys {
		ks = append(ks, hex.EncodeToString(k))
	}
	sum := sha256.Sum256([]byte(url + "\n" + strings.Join(ks, ",")))
	return &catalog.Source{
		URL:        url,
		CacheDir:   filepath.Join(cache, "raccoonci", "catalog", hex.EncodeToString(sum[:6])),
		PublicKeys: keys,
	}, nil
}

// loadCatalog assembles the catalog. A pull that fails is reported on stderr
// and never stops anything: the cached or built-in catalog is used instead.
func loadCatalog(f catalogFlags) (*catalog.Catalog, error) {
	cat := catalog.Default()
	src, err := f.source()
	if err != nil {
		return nil, err
	}
	if src != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var st catalog.Status
		cat, st = src.Load(ctx, cat, false)
		if st.Err != nil {
			fmt.Fprintf(os.Stderr, "raccoonci: catalog: %v (using the %s catalog)\n", st.Err, st.Origin)
		}
	}
	if f.file != "" {
		b, err := os.ReadFile(f.file)
		if err != nil {
			return nil, err
		}
		if cat, err = cat.Overlay(b); err != nil {
			return nil, fmt.Errorf("%s: %w", f.file, err)
		}
	}
	return cat, nil
}

// updateCatalog implements `raccoonci catalog update`: ask the feed now,
// whatever the cache says.
func updateCatalog(f catalogFlags) error {
	src, err := f.source()
	if err != nil {
		return err
	}
	if src == nil {
		return errors.New("no catalog feed: set -catalog-url or RACCOONCI_CATALOG_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cat, st := src.Load(ctx, catalog.Default(), true)
	if st.Err != nil {
		return fmt.Errorf("%v (kept the %s catalog)", st.Err, st.Origin)
	}
	fmt.Printf("catalog: %s (fetched %s)\n", st.Origin, st.Fetched.Local().Format(time.DateTime))
	fmt.Printf("  %d toolchains, %d tools, %d packages, %d actions, %d images\n",
		len(cat.Toolchains), len(cat.Tools), len(cat.Packages), len(cat.Actions), len(cat.Images))
	return nil
}

// catalogKeygen implements `raccoonci catalog keygen -out FILE`: a new signing
// key pair. The private key goes to FILE (0600) and is never printed; only the
// public key is shown.
func catalogKeygen(args []string) error {
	fs := flag.NewFlagSet("catalog keygen", flag.ContinueOnError)
	out := fs.String("out", "", "file to write the private key to (created 0600, must not exist)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || fs.NArg() != 0 {
		return errors.New("usage: raccoonci catalog keygen -out FILE")
	}
	pub, priv, err := catalog.GenerateKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(priv + "\n"); err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err != nil {
		os.Remove(*out)
		return err
	}
	fmt.Printf("private key written to %s (keep it in CI secrets as %s, not in a repository)\n", *out, signingKeyEnv)
	fmt.Printf("public key: %s\n", pub)
	return nil
}

// signingKeyEnv is the only place a signing key is read from: never a command
// line argument, which would show in process lists and shell history.
const signingKeyEnv = "RACCOONCI_CATALOG_SIGNING_KEY"

// catalogSign implements `raccoonci catalog sign FILE`: write FILE.sig.
func catalogSign(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: raccoonci catalog sign FILE (the private key is read from " + signingKeyEnv + ")")
	}
	key := os.Getenv(signingKeyEnv)
	if key == "" {
		return errors.New(signingKeyEnv + " is not set")
	}
	priv, err := catalog.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("%s: %w", signingKeyEnv, err)
	}
	body, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	// Refuse to sign something no client would accept.
	if _, err := catalog.Default().Overlay(body); err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	if err := os.WriteFile(args[0]+".sig", []byte(catalog.Sign(priv, body)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s -> %s.sig (public key %s)\n", args[0], args[0], catalog.PublicKeyOf(priv))
	return nil
}
