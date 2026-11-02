package main

import (
	"archive/tar"
	"compress/gzip"
	"github.com/Raccoon-CI/raccoonci/internal/catalog"
	"github.com/Raccoon-CI/raccoonci/internal/engine"
	"github.com/Raccoon-CI/raccoonci/internal/envsnap"
	"github.com/Raccoon-CI/raccoonci/internal/instabox"
	"github.com/Raccoon-CI/raccoonci/internal/runner"
	"github.com/Raccoon-CI/raccoonci/internal/wire"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func snapRun(t *testing.T, lock string) (*remoteRun, *wire.Reader) {
	t.Helper()
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "go.sum"), []byte(lock), 0o644)
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	idx, err := envsnap.Open(filepath.Join(t.TempDir(), "idx.json"), 5)
	if err != nil {
		t.Fatal(err)
	}
	rr := &remoteRun{provider: &instabox.Provider{Snapshot: "sn_base"}, snaps: idx, cat: catalog.Default(),
		repoFS: root.FS(), trusted: true}
	reader := &wire.Reader{Event: func(runner.Event) {}}
	reader.Write([]byte(`{"result":{"conclusion":"success","outputs":{}}}` + "\n"))
	return rr, reader
}

func TestOnlyTheFirstSuccessfulMachinePerKeyIsSnapshotted(t *testing.T) {
	rr, reader := snapRun(t, "a v1 h1:x\n")
	inst := engine.RemoteInstance{Name: "t", Toolchains: []string{"go"}}
	key, from := rr.restore(inst)
	if key == "" || from != "" {
		t.Fatalf("key %q from %q", key, from)
	}
	name := rr.keep(key, reader)
	if !strings.HasPrefix(name, "rc-env-"+key[:12]) {
		t.Fatalf("name %q", name)
	}
	if again := rr.keep(key, reader); again != "" {
		t.Errorf("a second machine for the same key would snapshot too: %q", again)
	}
	// Once saved, the next run restores it, and a changed lockfile misses.
	rr.snaps.Put(key, "sn_env", "sn_base")
	rr.claimed = nil
	if _, from := rr.restore(inst); from != "sn_env" {
		t.Errorf("restore = %q", from)
	}
	rr2, _ := snapRun(t, "a v1 h1:CHANGED\n")
	if k2, _ := rr2.restore(inst); k2 == key {
		t.Error("a changed go.sum kept the same key")
	}
}

func TestNoSnapshotFromAnUntrustedOrRiskyRun(t *testing.T) {
	inst := engine.RemoteInstance{Name: "t", Toolchains: []string{"go"}}
	for name, tweak := range map[string]func(*remoteRun){
		"fork pull request": func(rr *remoteRun) { rr.trusted = false },
		"secrets present":   func(rr *remoteRun) { rr.secrets = map[string]string{"TOKEN": "x"} },
	} {
		rr, reader := snapRun(t, "a\n")
		tweak(rr)
		key, _ := rr.restore(inst)
		if got := rr.keep(key, reader); got != "" {
			t.Errorf("%s: snapshot %q", name, got)
		}
	}
	// A failed job leaves a half-installed environment.
	rr, _ := snapRun(t, "a\n")
	failed := &wire.Reader{Event: func(runner.Event) {}}
	failed.Write([]byte(`{"result":{"conclusion":"failure","outputs":{}}}` + "\n"))
	key, _ := rr.restore(inst)
	if got := rr.keep(key, failed); got != "" {
		t.Errorf("failed job: snapshot %q", got)
	}
	// No toolchain, no lockfile, or no index: nothing to restore.
	if k, _ := rr.restore(engine.RemoteInstance{Name: "t"}); k != "" {
		t.Errorf("an instance with no toolchain got key %q", k)
	}
	rr.snaps = nil
	if k, _ := rr.restore(inst); k != "" {
		t.Errorf("snapshots off, key %q", k)
	}
}

func TestSplitRemote(t *testing.T) {
	rf, rest, err := splitRemote([]string{"-on", "instabox", "-job", "build", "--listen=127.0.0.1:1", "-debug",
		"-controller-url", "https://x.example", "-snapshot=sn_1", "-install", "none", "w.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if rf.on != "instabox" || rf.listen != "127.0.0.1:1" || rf.controllerURL != "https://x.example" || rf.snapshot != "sn_1" {
		t.Errorf("%+v", rf)
	}
	want := []string{"-job", "build", "-debug", "-install", "none", "w.yml"}
	if !slices.Equal(rest, want) {
		t.Errorf("rest = %v, want %v", rest, want)
	}
	// A flag without a value must not swallow the next word.
	_, rest, err = splitRemote([]string{"-on", "instabox", "-affected", "-env-snapshots", "5", "-only-if-triggered", "w.yml"})
	if err != nil || !slices.Equal(rest, []string{"-affected", "-env-snapshots", "5", "-only-if-triggered", "w.yml"}) {
		t.Errorf("rest = %v, err = %v", rest, err)
	}
	if _, _, err := splitRemote([]string{"-on"}); err == nil {
		t.Error("a flag with no value was accepted")
	}
}

func tarNames(t *testing.T, repo string) map[string]bool {
	t.Helper()
	p, err := makeSourceTar(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names[strings.TrimSuffix(h.Name, "/")] = true
	}
}

// The source package leaves out what git ignores (a clone on GitHub has none
// of it) but keeps .git, tracked files, and new files not yet committed.
func TestSourceTarLeavesOutIgnoredFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(repo, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write(".gitignore", "node_modules/\n*.log\n")
	write("main.go", "package main\n")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	write("node_modules/x/index.js", "x")
	write("deep/build.log", "log")
	write("new.go", "package main\n") // not committed, not ignored

	names := tarNames(t, repo)
	for _, want := range []string{"src/main.go", "src/.gitignore", "src/new.go", "src/.git/HEAD"} {
		if !names[want] {
			t.Errorf("%s is missing from the package", want)
		}
	}
	for _, bad := range []string{"src/node_modules", "src/node_modules/x/index.js", "src/deep/build.log"} {
		if names[bad] {
			t.Errorf("%s should have been left out", bad)
		}
	}
}

// Outside a git checkout nothing is left out.
func TestSourceTarWithoutGitKeepsEverything(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "node_modules"), 0o755)
	os.WriteFile(filepath.Join(repo, "node_modules", "a.js"), []byte("x"), 0o644)
	if !tarNames(t, repo)["src/node_modules/a.js"] {
		t.Error("a tree that is not a git checkout lost files")
	}
}
