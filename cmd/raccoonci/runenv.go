package main

import (
	"fmt"
	"os"
	"path/filepath"

	"raccoonci/internal/catalog"
	"raccoonci/internal/nodejs"
	"raccoonci/internal/provision"
)

// runEnv is what a machine needs to run job instances besides the workflow:
// the Node.js runtimes for actions, the tool cache and the provisioner. `run`
// and `run-instance` build it the same way.
type runEnv struct {
	cacheDir  string // <user cache>/raccoonci
	node      *nodejs.Runtime
	nodeMusl  *nodejs.Runtime // for job containers on musl images (Alpine)
	toolCache string
	prov      *provision.Provisioner // nil with -install none
}

func validInstall(install string) error {
	switch install {
	case "none", "toolchains", "all":
		return nil
	}
	return fmt.Errorf("-install %q: want none, toolchains or all", install)
}

func newRunEnv(cat *catalog.Catalog, install string) (*runEnv, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	rt := &runEnv{cacheDir: filepath.Join(cache, "raccoonci")}
	rt.node = nodejs.New(filepath.Join(rt.cacheDir, "node"))
	rt.node.Catalog = cat
	// Inside node.Dir, so the mount that gives containers the Node.js
	// runtimes covers it.
	rt.nodeMusl = nodejs.NewMusl(filepath.Join(rt.node.Dir, "musl"))
	rt.nodeMusl.Catalog = cat
	rt.toolCache = filepath.Join(rt.cacheDir, "toolcache")
	switch install {
	case "none":
	case "toolchains", "all":
		rt.prov = &provision.Provisioner{ToolCache: rt.toolCache, Catalog: cat, Installers: provision.DefaultInstallers(nil)}
		if install == "all" {
			if rt.prov.Apt, err = provision.NewApt(); err != nil {
				fmt.Fprintf(os.Stderr, "raccoonci: -install=all: %v; missing packages will only be reported\n", err)
			}
		}
	default:
		return nil, fmt.Errorf("-install %q: want none, toolchains or all", install)
	}
	return rt, nil
}
