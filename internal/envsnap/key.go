// Package envsnap decides when a snapshot of a prepared environment can be
// reused: a project's dependencies are installed once, the machine is
// snapshotted, and later runs start from the snapshot until the dependencies
// change.
//
// It holds the provider-independent parts: the key a snapshot is filed under
// (Compute) and the register of keys to snapshot ids (Index). Taking and
// restoring snapshots is the provider's job.
//
// Rules for whoever takes a snapshot, because a snapshot is trusted by every
// later run that has the same key:
//   - only from a trusted ref (a branch of the repository itself, never a fork
//     pull request: its install scripts could tamper with the environment and
//     a later, legitimate commit with the same lockfile would inherit that);
//   - from a machine with no secrets on its disks (no registry tokens, no
//     GITHUB_TOKEN written by a step): a snapshot copies sign-ins too.
package envsnap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"

	"raccoonci/internal/catalog"
)

// maxLockfile bounds reads of repo files, which come from untrusted pull requests.
const maxLockfile = 64 << 20

// ErrUnpinned means a toolchain has a manifest but no lockfile, so nothing says
// when its dependencies change; such a project is not snapshotted.
var ErrUnpinned = errors.New("dependencies are not pinned by a lockfile")

// maxWalk bounds the directory walk that looks for nested projects.
const maxWalk = 20000

// skipDirs hold installed, vendored or VCS data, not the project's own manifests.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true, "target": true}

// ErrNothingToSnapshot means no demanded toolchain has dependencies in the repo.
var ErrNothingToSnapshot = errors.New("no lockfile among the toolchains this job uses")

// Part is one lockfile that went into a key.
type Part struct {
	Toolchain string
	File      string
	SHA256    string
}

// Key identifies the environment a snapshot holds.
type Key struct {
	Hash  string // hex, the register's key
	Parts []Part
}

// Compute derives the key for a job that uses toolchains, on a machine built
// from base (the provider's identity of its image, e.g. template id, version
// and digest: a new template means new system packages). repo is read at its
// root only.
//
// Toolchain versions are not part of the key. Toolchains come from the machine
// or are installed on demand; the snapshot is for what they install.
func Compute(repo fs.FS, cat *catalog.Catalog, base string, toolchains []string) (Key, error) {
	names := slices.Clone(toolchains)
	slices.Sort(names)
	names = slices.Compact(names)

	if err := checkNested(repo, cat, names); err != nil {
		return Key{}, err
	}

	var parts []Part
	for _, name := range names {
		def := cat.Toolchains[name]
		if def == nil {
			continue
		}
		found := false
		for _, f := range def.Lockfiles {
			sum, ok, err := hashFile(repo, f)
			if err != nil {
				return Key{}, fmt.Errorf("%s: %w", f, err)
			}
			if ok {
				parts = append(parts, Part{Toolchain: name, File: f, SHA256: sum})
				found = true
			}
		}
		if found {
			continue
		}
		// No lockfile. That is fine if the repo has no dependencies for this
		// toolchain at all, and unsafe if it has a manifest.
		for _, m := range def.Manifests {
			if _, err := fs.Stat(repo, m); err == nil {
				return Key{}, fmt.Errorf("%w: %s has %s but no lockfile", ErrUnpinned, cat.Title(name), m)
			}
		}
	}
	if len(parts) == 0 {
		return Key{}, ErrNothingToSnapshot
	}
	slices.SortFunc(parts, func(a, b Part) int {
		return strings.Compare(a.Toolchain+"\x00"+a.File, b.Toolchain+"\x00"+b.File)
	})

	h := sha256.New()
	fmt.Fprintf(h, "raccoonci-envsnap/1\nbase\t%s\n", base)
	for _, p := range parts {
		fmt.Fprintf(h, "%s\t%s\t%s\n", p.Toolchain, p.File, p.SHA256)
	}
	return Key{Hash: hex.EncodeToString(h.Sum(nil)), Parts: parts}, nil
}

// checkNested fails with ErrUnpinned when a used toolchain has a manifest or
// lockfile below the repo root. The key covers the root only, so a change in
// such a subdirectory would go unnoticed and a stale snapshot be reused.
func checkNested(repo fs.FS, cat *catalog.Catalog, names []string) error {
	owner := map[string]string{} // file name -> toolchain
	for _, name := range names {
		def := cat.Toolchains[name]
		if def == nil {
			continue
		}
		for _, f := range def.Manifests {
			owner[f] = name
		}
		for _, f := range def.Lockfiles {
			owner[f] = name
		}
	}
	if len(owner) == 0 {
		return nil
	}
	seen := 0
	var found error
	err := fs.WalkDir(repo, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if seen++; seen > maxWalk {
			return fmt.Errorf("%w: more than %d entries to search for nested projects", ErrUnpinned, maxWalk)
		}
		if d.IsDir() {
			if p != "." && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if tc, ok := owner[d.Name()]; ok && path.Dir(p) != "." {
			found = fmt.Errorf("%w: %s has %s below the repository root, which the key does not cover", ErrUnpinned, cat.Title(tc), p)
			return fs.SkipAll
		}
		return nil
	})
	if found != nil {
		return found
	}
	return err
}

func hashFile(repo fs.FS, name string) (sum string, ok bool, err error) {
	f, err := repo.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxLockfile+1))
	if err != nil {
		return "", false, err
	}
	if n > maxLockfile {
		return "", false, fmt.Errorf("larger than %d bytes", maxLockfile)
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}
