package main

import (
	"os"
	"path/filepath"
	"raccoonci/internal/catalog"
	"raccoonci/internal/engine"
	"raccoonci/internal/envsnap"
	"raccoonci/internal/instabox"
	"raccoonci/internal/runner"
	"slices"
	"strings"
	"testing"
)

func snapRun(t *testing.T, lock string) (*remoteRun, *wireReader) {
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
	reader := &wireReader{event: func(runner.Event) {}}
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
	failed := &wireReader{event: func(runner.Event) {}}
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
	if _, _, err := splitRemote([]string{"-on"}); err == nil {
		t.Error("a flag with no value was accepted")
	}
}
