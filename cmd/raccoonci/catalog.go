package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"raccoonci/internal/catalog"
)

// catalogFlags say where the catalog comes from, in layers: the built-in
// one, then the one pulled from URL (cached on disk), then the local file.
type catalogFlags struct {
	file string
	url  string
	key  string
}

func (f catalogFlags) source() (*catalog.Source, error) {
	if f.url == "" {
		return nil, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	// One cache per feed and key, so switching feeds can't mix their copies.
	sum := sha256.Sum256([]byte(f.url + "\n" + f.key))
	src := &catalog.Source{
		URL:      f.url,
		CacheDir: filepath.Join(cache, "raccoonci", "catalog", hex.EncodeToString(sum[:6])),
	}
	if f.key != "" {
		if src.PublicKey, err = catalog.ParsePublicKey(f.key); err != nil {
			return nil, fmt.Errorf("-catalog-key: %w", err)
		}
	}
	return src, nil
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
