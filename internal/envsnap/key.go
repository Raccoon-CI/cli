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
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/Raccoon-CI/raccoonci/internal/affected"
	"github.com/Raccoon-CI/raccoonci/pkg/catalog"
)

// maxLockfile bounds reads of repo files, which come from untrusted pull requests.
const maxLockfile = 64 << 20

// ErrUnpinned means a toolchain has a manifest but no lockfile, so nothing says
// when its dependencies change; such a project is not snapshotted.
var ErrUnpinned = errors.New("dependencies are not pinned by a lockfile")

// maxWalk bounds the directory walk that looks for projects.
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
// and digest: a new template means new system packages).
//
// Every project of a used toolchain counts, in the root or below it: a project
// is a directory holding a manifest or a lockfile of the toolchain (the
// catalog's lists), and its lockfiles go into the key under their path from
// the root. A manifest with no lockfile beside it is not pinned (ErrUnpinned),
// wherever it is. The key covers all projects of the toolchain, not only the
// ones the job works in: that can rebuild a snapshot more often than needed
// but never reuses a stale one. A lockfile of an ancestor directory pins a
// manifest only if the manifest is a member of a workspace (npm, pnpm, yarn,
// Cargo), whose one lockfile covers its members; otherwise nothing says the
// two belong together.
//
// Toolchain versions are not part of the key. Toolchains come from the machine
// or are installed on demand; the snapshot is for what they install.
func Compute(repo fs.FS, cat *catalog.Catalog, base string, toolchains []string) (Key, error) {
	names := slices.Clone(toolchains)
	slices.Sort(names)
	names = slices.Compact(names)

	projects, err := findProjects(repo, cat, names)
	if err != nil {
		return Key{}, err
	}

	var members map[string]map[string]bool // toolchain -> workspace member dirs, found when first needed
	var parts []Part
	for _, name := range names {
		dirs := projects[name]
		for _, dir := range slices.Sorted(maps.Keys(dirs)) {
			pr := dirs[dir]
			if len(pr.locks) == 0 {
				// A manifest and nothing that says when its dependencies change,
				// unless it is a member of a workspace whose lockfile, in a
				// directory above, covers all its members.
				if members == nil {
					members = workspaceMembers(repo)
				}
				if members[name][dir] && lockedAbove(dirs, dir) {
					continue
				}
				return Key{}, fmt.Errorf("%w: %s has %s but no lockfile", ErrUnpinned, cat.Title(name), path.Join(dir, pr.manifests[0]))
			}
			for _, f := range pr.locks {
				file := path.Join(dir, f)
				sum, ok, err := hashFile(repo, file)
				if err != nil {
					return Key{}, fmt.Errorf("%s: %w", file, err)
				}
				if ok {
					parts = append(parts, Part{Toolchain: name, File: file, SHA256: sum})
				}
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

// workspaceKinds maps the package kinds package affected finds to the
// catalog's toolchain names.
var workspaceKinds = map[string]string{"npm": "node", "cargo": "rust"}

// workspaceMembers lists, per toolchain, the directories that are members of a
// workspace of the repository (npm, pnpm and yarn workspaces, Cargo's). Their
// dependencies are locked by the workspace's one lockfile.
func workspaceMembers(repo fs.FS) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, p := range affected.Packages(repo) {
		tc, ok := workspaceKinds[p.Kind]
		if !ok {
			continue
		}
		if out[tc] == nil {
			out[tc] = map[string]bool{}
		}
		out[tc][p.Dir] = true
	}
	return out
}

// lockedAbove reports whether a directory above dir holds a lockfile of the
// toolchain (dirs are its projects).
func lockedAbove(dirs map[string]*project, dir string) bool {
	for d := path.Dir(dir); ; d = path.Dir(d) {
		if pr := dirs[d]; pr != nil && len(pr.locks) > 0 {
			return true
		}
		if d == "." {
			return false
		}
	}
}

// project is what one directory holds of one toolchain.
type project struct {
	manifests, locks []string // file names
}

// findProjects walks the repo (skipping installed and vendored code) and
// returns, per used toolchain, the directories that hold its manifests or
// lockfiles. A walk that passes maxWalk entries gives up with ErrUnpinned.
func findProjects(repo fs.FS, cat *catalog.Catalog, names []string) (map[string]map[string]*project, error) {
	type role struct {
		toolchain string
		lock      bool
	}
	owner := map[string][]role{} // file name -> who claims it
	for _, name := range names {
		def := cat.Toolchains[name]
		if def == nil {
			continue
		}
		for _, f := range def.Manifests {
			owner[f] = append(owner[f], role{name, false})
		}
		for _, f := range def.Lockfiles {
			owner[f] = append(owner[f], role{name, true})
		}
	}
	out := map[string]map[string]*project{}
	if len(owner) == 0 {
		return out, nil
	}
	seen := 0
	err := fs.WalkDir(repo, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if seen++; seen > maxWalk {
			return fmt.Errorf("%w: more than %d entries to search for projects", ErrUnpinned, maxWalk)
		}
		if d.IsDir() {
			if p != "." && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		for _, r := range owner[d.Name()] {
			if out[r.toolchain] == nil {
				out[r.toolchain] = map[string]*project{}
			}
			dir := path.Dir(p)
			pr := out[r.toolchain][dir]
			if pr == nil {
				pr = &project{}
				out[r.toolchain][dir] = pr
			}
			if r.lock {
				pr.locks = append(pr.locks, d.Name())
			} else {
				pr.manifests = append(pr.manifests, d.Name())
			}
		}
		return nil
	})
	return out, err
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
