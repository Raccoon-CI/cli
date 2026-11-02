package main

import (
	"raccoonci/internal/catalog"
	"raccoonci/internal/nodejs"
	"raccoonci/internal/provision"
	"raccoonci/internal/runnercmd"
)

// runEnv is runnercmd.RunEnv under the names `run` has always used for its
// fields. It is an adapter that exists so that run.go did not have to change
// when the machine side moved out of this package; it goes when `run` itself
// moves to the runner (TODO D.5, decision 8).
type runEnv struct {
	cacheDir  string
	node      *nodejs.Runtime
	nodeMusl  *nodejs.Runtime
	toolCache string
	prov      *provision.Provisioner
}

func validInstall(install string) error { return runnercmd.ValidInstall(install) }

func newRunEnv(cat *catalog.Catalog, install string) (*runEnv, error) {
	e, err := runnercmd.NewRunEnv(cat, install)
	if err != nil {
		return nil, err
	}
	return &runEnv{cacheDir: e.CacheDir, node: e.Node, nodeMusl: e.NodeMusl, toolCache: e.ToolCache, prov: e.Prov}, nil
}
