package envsnap

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Raccoon-CI/lib/pkg/catalog"
)

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }

func key(t *testing.T, repo fstest.MapFS, base string, tcs ...string) (Key, error) {
	t.Helper()
	return Compute(repo, catalog.Default(), base, tcs)
}

func TestKeyFollowsTheLockfileNotTheSource(t *testing.T) {
	repo := fstest.MapFS{
		"go.mod":  {Data: []byte("module x\n")},
		"go.sum":  {Data: []byte("a v1 h1:aaa\n")},
		"main.go": {Data: []byte("package main\n")},
	}
	k1, err := key(t, repo, "universal@1", "go")
	if err != nil || k1.Hash == "" {
		t.Fatal(k1, err)
	}

	// Source changes and the order or repetition of toolchains do not matter.
	repo["main.go"] = &fstest.MapFile{Data: []byte("package main // edited\n")}
	if k2, _ := key(t, repo, "universal@1", "go", "go"); k2.Hash != k1.Hash {
		t.Error("editing source changed the key")
	}
	// A dependency change does.
	repo["go.sum"] = &fstest.MapFile{Data: []byte("a v2 h1:bbb\n")}
	if k3, _ := key(t, repo, "universal@1", "go"); k3.Hash == k1.Hash {
		t.Error("a new go.sum kept the key")
	}
	// So does a new machine image.
	repo["go.sum"] = &fstest.MapFile{Data: []byte("a v1 h1:aaa\n")}
	if k4, _ := key(t, repo, "universal@2", "go"); k4.Hash == k1.Hash {
		t.Error("a new template kept the key")
	}
	if k5, _ := key(t, repo, "universal@1", "go"); k5.Hash != k1.Hash {
		t.Error("the same inputs gave another key")
	}
}

func TestKeyCoversEveryToolchainTheJobUses(t *testing.T) {
	repo := fstest.MapFS{
		"go.mod": {Data: []byte("module x\n")}, "go.sum": {Data: []byte("s\n")},
		"package.json": {Data: []byte("{}")}, "package-lock.json": {Data: []byte("l1")},
	}
	goOnly, _ := key(t, repo, "b", "go")
	both, err := key(t, repo, "b", "go", "node")
	if err != nil || both.Hash == goOnly.Hash || len(both.Parts) != 2 {
		t.Fatalf("%+v %v", both, err)
	}
	repo["package-lock.json"] = &fstest.MapFile{Data: []byte("l2")}
	if k, _ := key(t, repo, "b", "go"); k.Hash != goOnly.Hash {
		t.Error("a Node lockfile changed the key of a Go-only job")
	}
	if k, _ := key(t, repo, "b", "go", "node"); k.Hash == both.Hash {
		t.Error("a Node lockfile change was missed")
	}
}

func TestUnpinnedDependenciesAreNotSnapshotted(t *testing.T) {
	// A manifest without a lockfile: nothing says when the dependencies change.
	repo := fstest.MapFS{"package.json": {Data: []byte("{}")}}
	if _, err := key(t, repo, "b", "node"); !errors.Is(err, ErrUnpinned) {
		t.Errorf("err = %v", err)
	}
	// Another toolchain's lockfile does not excuse it.
	repo["go.sum"] = &fstest.MapFile{Data: []byte("s")}
	if _, err := key(t, repo, "b", "go", "node"); !errors.Is(err, ErrUnpinned) {
		t.Errorf("err = %v", err)
	}
	// A toolchain with no dependencies in the repo needs no lockfile.
	if _, err := key(t, repo, "b", "go", "python"); err != nil {
		t.Errorf("python without any manifest: %v", err)
	}
	// Nothing to pin at all.
	if _, err := key(t, fstest.MapFS{"README": {Data: []byte("x")}}, "b", "go", "node"); !errors.Is(err, ErrNothingToSnapshot) {
		t.Errorf("err = %v", err)
	}
}

func TestIndexLookupEvictionAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "snapshots.json")
	x, err := Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	n := 0
	x.now = func() time.Time { n++; return t0.Add(time.Duration(n) * time.Second) }

	for _, k := range []string{"a", "b"} {
		if ev, err := x.Put(k, "sn_"+k, "base"); err != nil || len(ev) != 0 {
			t.Fatal(ev, err)
		}
	}
	if e, ok := x.Lookup("a"); !ok || e.Snapshot != "sn_a" { // a is now newer than b
		t.Fatal(e, ok)
	}
	ev, err := x.Put("c", "sn_c", "base")
	if err != nil || len(ev) != 1 || ev[0].Key != "b" {
		t.Fatalf("evicted %+v, %v: want b, the least recently used", ev, err)
	}
	if _, ok := x.Lookup("b"); ok {
		t.Error("b is still filed")
	}

	// Replacing a key hands back the old snapshot to delete.
	if ev, _ := x.Put("a", "sn_a2", "base"); len(ev) != 1 || ev[0].Snapshot != "sn_a" {
		t.Errorf("replaced entry not returned: %+v", ev)
	}

	y, err := Open(path, 2)
	if err != nil || y.Len() != 2 {
		t.Fatalf("reopened: %d entries, %v", y.Len(), err)
	}
	if e, ok := y.Lookup("a"); !ok || e.Snapshot != "sn_a2" {
		t.Errorf("a after reopening: %+v", e)
	}
	if err := y.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if z, _ := Open(path, 2); z.Len() != 1 {
		t.Errorf("Remove was not saved: %d", z.Len())
	}
}

func TestOpenRejectsACorruptIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	for _, bad := range []string{"not json", `[{"key":"","snapshot":"x"}]`, `[null]`} {
		if err := writeFile(path, bad); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, 3); err == nil || !strings.Contains(err.Error(), "snapshot index") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// Dependencies in a subdirectory are part of the key: its lockfile is hashed
// under its path, so a change there gives a new key, and a project in a
// subdirectory no longer rules the repository out.
func TestSubdirectoryProjectsAreInTheKey(t *testing.T) {
	repo := func(webLock string) fstest.MapFS {
		return fstest.MapFS{
			"go.mod": {Data: []byte("module x\n")}, "go.sum": {Data: []byte("s\n")},
			"web/package.json":      {Data: []byte("{}")},
			"web/package-lock.json": {Data: []byte(webLock)},
		}
	}
	a, err := key(t, repo("1"), "b", "go", "node")
	if err != nil {
		t.Fatalf("a pinned subdirectory project should work: %v", err)
	}
	var files []string
	for _, p := range a.Parts {
		files = append(files, p.File)
	}
	if !slices.Equal(files, []string{"go.sum", "web/package-lock.json"}) {
		t.Errorf("parts = %v", files)
	}
	if b, _ := key(t, repo("2"), "b", "go", "node"); b.Hash == a.Hash {
		t.Error("a changed lockfile in a subdirectory kept the key")
	}
	if c, _ := key(t, repo("1"), "b", "go", "node"); c.Hash != a.Hash {
		t.Error("the same files gave another key")
	}
	// The same lockfile moved to another directory is another environment.
	moved := fstest.MapFS{
		"go.mod": {Data: []byte("module x\n")}, "go.sum": {Data: []byte("s\n")},
		"app/package.json":      {Data: []byte("{}")},
		"app/package-lock.json": {Data: []byte("1")},
	}
	if m, _ := key(t, moved, "b", "go", "node"); m.Hash == a.Hash {
		t.Error("the path is not in the key")
	}
}

// The members of a workspace have no lockfile of their own: the root's covers
// them, so the project can be snapshotted, and the key follows that lockfile.
func TestWorkspaceMembersAreLockedByTheRootLockfile(t *testing.T) {
	npm := func(lock string) fstest.MapFS {
		return fstest.MapFS{
			"package.json":             {Data: []byte(`{"name":"root","workspaces":["packages/*"]}`)},
			"package-lock.json":        {Data: []byte(lock)},
			"packages/a/package.json":  {Data: []byte(`{"name":"a"}`)},
			"packages/b/package.json":  {Data: []byte(`{"name":"b","dependencies":{"a":"*"}}`)},
			"tools/other/package.json": {Data: []byte(`{"name":"other"}`)}, // not a member
		}
	}
	// With a package that is not a member, nothing says its dependencies are locked.
	if _, err := key(t, npm("1"), "b", "node"); !errors.Is(err, ErrUnpinned) {
		t.Errorf("a non-member below the root lockfile: err = %v, want ErrUnpinned", err)
	}
	repo := npm("1")
	delete(repo, "tools/other/package.json")
	k1, err := key(t, repo, "b", "node")
	if err != nil {
		t.Fatalf("workspace members: %v", err)
	}
	repo2 := npm("2")
	delete(repo2, "tools/other/package.json")
	if k2, _ := key(t, repo2, "b", "node"); k2.Hash == k1.Hash {
		t.Error("a changed root lockfile did not change the key")
	}

	cargo := fstest.MapFS{
		"Cargo.toml":             {Data: []byte("[workspace]\nmembers = [\"crates/*\"]\n")},
		"Cargo.lock":             {Data: []byte("x")},
		"crates/a/Cargo.toml":    {Data: []byte("[package]\nname = \"a\"\n")},
		"crates/b/Cargo.toml":    {Data: []byte("[package]\nname = \"b\"\n")},
		"vendorish/z/Cargo.toml": {Data: []byte("[package]\nname = \"z\"\n")},
	}
	if _, err := key(t, cargo, "b", "rust"); !errors.Is(err, ErrUnpinned) {
		t.Errorf("a crate outside the workspace: err = %v, want ErrUnpinned", err)
	}
	delete(cargo, "vendorish/z/Cargo.toml")
	if _, err := key(t, cargo, "b", "rust"); err != nil {
		t.Errorf("workspace crates: %v", err)
	}
}

func TestSubdirectoryProjectsNeedTheirOwnLockfile(t *testing.T) {
	base := func() fstest.MapFS {
		return fstest.MapFS{"go.mod": {Data: []byte("module x\n")}, "go.sum": {Data: []byte("s\n")}}
	}
	for _, sub := range []string{"web/package.json", "tools/go.mod", "a/b/Cargo.toml"} {
		repo := base()
		repo[sub] = &fstest.MapFile{Data: []byte("{}")}
		if _, err := key(t, repo, "b", "go", "node", "rust"); !errors.Is(err, ErrUnpinned) {
			t.Errorf("%s: err = %v, want ErrUnpinned", sub, err)
		}
	}
	// A lockfile in the root does not pin a manifest below it.
	repo := base()
	repo["package-lock.json"] = &fstest.MapFile{Data: []byte("{}")}
	repo["web/package.json"] = &fstest.MapFile{Data: []byte("{}")}
	if _, err := key(t, repo, "b", "go", "node"); !errors.Is(err, ErrUnpinned) {
		t.Errorf("manifest below a root lockfile: err = %v, want ErrUnpinned", err)
	}
	// A lockfile with no manifest beside it is still a lockfile to hash.
	repo = base()
	repo["web/package-lock.json"] = &fstest.MapFile{Data: []byte("{}")}
	if _, err := key(t, repo, "b", "go", "node"); err != nil {
		t.Errorf("lockfile alone: %v", err)
	}
	// Installed or vendored code is not the project's own.
	repo = fstest.MapFS{
		"go.mod": base()["go.mod"], "go.sum": base()["go.sum"],
		"node_modules/x/package.json": {Data: []byte("{}")},
		".git/config":                 {Data: []byte("")},
	}
	if _, err := key(t, repo, "b", "go", "node"); err != nil {
		t.Errorf("node_modules must be ignored: %v", err)
	}
	// A toolchain the job does not use does not matter.
	repo = base()
	repo["web/package.json"] = &fstest.MapFile{Data: []byte("{}")}
	if _, err := key(t, repo, "b", "go"); err != nil {
		t.Errorf("unused toolchain's subdirectory: %v", err)
	}
	// A project with a manifest and no dependencies at all has nothing to snapshot.
	if _, err := key(t, fstest.MapFS{"x/go.sum": {Data: []byte("")}}, "b", "node"); !errors.Is(err, ErrNothingToSnapshot) {
		t.Errorf("err = %v, want ErrNothingToSnapshot", err)
	}
}

// layered returns the built-in catalog with a piece of YAML laid over it.
func layered(t *testing.T, over string) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Default().Overlay([]byte(over))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Which directories a scan for projects does not enter is the catalog's
// `skip-dirs`, not a list in this package.
func TestSkippedDirectoriesComeFromTheCatalog(t *testing.T) {
	repo := fstest.MapFS{
		"go.mod": {Data: []byte("module x\n")}, "go.sum": {Data: []byte("s\n")},
		"node_modules/x/package.json": {Data: []byte("{}")},
		"dist/a/package.json":         {Data: []byte("{}")},
	}
	compute := func(c *catalog.Catalog) error {
		_, err := Compute(repo, c, "b", []string{"go", "node"})
		return err
	}
	// Built in: node_modules is skipped, dist is not, so its manifest has no lockfile.
	if err := compute(catalog.Default()); !errors.Is(err, ErrUnpinned) {
		t.Errorf("built-in skip-dirs: err = %v, want ErrUnpinned (dist/a/package.json)", err)
	}
	// A catalog that also skips dist: nothing is left unpinned.
	if err := compute(layered(t, "skip-dirs: [.git, node_modules, dist]\n")); err != nil {
		t.Errorf("dist skipped by the catalog: %v", err)
	}
	// A catalog that does not skip node_modules: its manifest counts now.
	if err := compute(layered(t, "skip-dirs: [.git, dist]\n")); !errors.Is(err, ErrUnpinned) {
		t.Errorf("node_modules no longer skipped: err = %v, want ErrUnpinned", err)
	}
}

// Which toolchains have workspaces, and of which kind, is the toolchain's
// `workspace-kind` in the catalog.
func TestWorkspaceKindsComeFromTheCatalog(t *testing.T) {
	npm := fstest.MapFS{
		"package.json":            {Data: []byte(`{"name":"root","workspaces":["packages/*"]}`)},
		"package-lock.json":       {Data: []byte("1")},
		"packages/a/package.json": {Data: []byte(`{"name":"a"}`)},
	}
	if _, err := Compute(npm, catalog.Default(), "b", []string{"node"}); err != nil {
		t.Fatalf("built in, npm workspace: %v", err)
	}
	noWorkspaces := layered(t, "toolchains:\n  node: {title: Node.js, installer: node, toolcache: node, commands: [node, npm, npx, corepack], manifests: [package.json], lockfiles: [package-lock.json]}\n")
	if _, err := Compute(npm, noWorkspaces, "b", []string{"node"}); !errors.Is(err, ErrUnpinned) {
		t.Errorf("node without a workspace kind: err = %v, want ErrUnpinned", err)
	}

	// Go modules of a go.work are not treated as workspace members by default:
	// a module below the root with no go.sum of its own is unpinned. A catalog
	// that gives Go the kind "go" (the name package affected gives Go modules)
	// treats them as members.
	goMods := fstest.MapFS{
		"go.work": {Data: []byte("go 1.24\n\nuse (\n\t.\n\t./tools\n)\n")},
		"go.mod":  {Data: []byte("module x\n")}, "go.sum": {Data: []byte("s\n")},
		"tools/go.mod": {Data: []byte("module x/tools\n")},
	}
	if _, err := Compute(goMods, catalog.Default(), "b", []string{"go"}); !errors.Is(err, ErrUnpinned) {
		t.Errorf("built in, go submodule: err = %v, want ErrUnpinned", err)
	}
	goKind := layered(t, "toolchains:\n  go: {title: Go, installer: go, toolcache: go, commands: [go, gofmt], manifests: [go.mod], lockfiles: [go.sum], workspace-kind: go}\n")
	if _, err := Compute(goMods, goKind, "b", []string{"go"}); err != nil {
		t.Errorf("go with workspace-kind go: %v", err)
	}
}
