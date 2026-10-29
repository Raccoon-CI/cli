package envsnap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"raccoonci/internal/catalog"
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
