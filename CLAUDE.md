# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

This is the **cli** repository of Raccoon CI (module `github.com/Raccoon-CI/cli`). It builds one binary, `raccoonci`, the user-facing command: `plan`, `match`, `affected`, `run`, `serve`, `agent`, `runner-snapshot`, `catalog …`, `runner-images update`. The machine-side binary `raccoonci-runner` lives in the runner repository; the server lives in the backend repository.

Raccoon CI is a self-hosted CI/CD platform. Users move their workflows from GitHub Actions onto compute they choose. The config format is GitHub Actions workflow YAML; migrating from GitHub must be seamless. Compute is pluggable (the user's own machines, then InstaBox, then cheap clouds); runners are Linux only for now, and InstaBox machines run Debian 13 (trixie), not Ubuntu.

There are two ways to run a workflow, and this repository holds the user-facing side of both. (1) The own engine (`raccoonci run`; the engine itself is in the runner repository). (2) The GitHub-triggered path (`raccoonci serve`, lib's `pkg/ghrunner`): GitHub still triggers and orchestrates the workflow, and a job whose `runs-on` has a `raccoon-*` label runs on an InstaBox machine through GitHub's own runner (the shape Blacksmith has). Which path is the product's main one is not decided: do not remove either, and do not call one of them "the" architecture.

## Depends on

Direction of dependencies: lib <- runner <- cli (and lib <- backend). This repository imports only public packages of the other two:

- **runner** (`github.com/Raccoon-CI/runner`): `pkg/actions`, `pkg/cliflag`, `pkg/cliutil`, `pkg/engine`, `pkg/runner`, `pkg/runnercmd`, `pkg/scan`.
- **lib** (`github.com/Raccoon-CI/lib`): `pkg/affected`, `catalog`, `cron`, `dispatch`, `expr`, `ghrunner`, `instabox`, `platform`, `runnerimages`, `sched`, `trigger`, `value`, `wire`, `workflow`.

**Never import the `internal/` packages of runner or lib** (runner: `internal/docker`, `local`, `mirror`, `missing`, `nodejs`, `provision`, `semver`; lib: `internal/azblob`, `twirp`). Go refuses it across modules; if something needed is internal, make it public in the owning repository instead of copying it here. This repository must never be imported by runner, lib or backend.

## Developing across the repositories

go.mod requires the published `lib v0.1.0` and `runner v0.1.0`, so `go build ./... && go test ./...` work on their own, and so does CI. To try a change to lib or runner together with this repository, put the checkouts side by side and add a `go.work` next to them with a `use` line for each directory (do not commit it). A release of lib or runner is a new tag there; bump the `require` here afterwards.

## Commands

```sh
make build                # builds dist/raccoonci (VERSION=x.y.z sets main.version)
make runner-binary        # static linux/amd64 raccoonci for the InstaBox runner snapshot
make test                 # go test ./...
make race                 # go test -race ./...
make vet                  # go vet ./...
make check                # vet + race — must pass before merging
go test ./internal/foo -run TestName -v   # run a single test
make runner-images        # refresh the snapshot of GitHub's Ubuntu runner images in ../lib (network)
```

Use `GOFLAGS=-p=1` when building and testing (memory budget).

## Layout

- `cmd/raccoonci/`: the only binary. Keep `main` thin: parse flags, wire dependencies, call into `internal/` and the runner's `pkg/runnercmd`.
- `internal/plan`: workflow → instances (matrix expansion, `runs-on` evaluation, availability).
- `internal/envsnap`: reuse of prepared-environment snapshots (lockfile hashes).
- Not here any more: `sched` (which job instances share a machine) and `ghrunner` (the GitHub-triggered path behind `raccoonci serve`) are `pkg/sched` and `pkg/ghrunner` in the lib repository.
- `docs/research/idle-runners`: the keep-alive model, simulator and numbers.
- `version` lives in `package main`, set through `-ldflags "-X main.version=..."` in the Makefile.

Documentation convention: English `x.md` plus Chinese `x.zh.md` (Simplified) for user-facing docs; CLAUDE.md is English; TODO.md is Chinese (history kept from the monorepo).

## Command notes

`raccoonci plan <workflow.yml>` parses a workflow and prints its jobs, matrix combinations, each run's `runs-on`, what each run needs installed, and warnings for OSes we can't provide. Use it to sanity-check the parser, evaluator and scanner against real workflow files.

`raccoonci run [-job id] [-secret NAME] [-var K=V] [-install none|toolchains|all] [-cache-protocol v1|v2] [-workdir dir] <workflow.yml>` runs a workflow on this machine. Actions are downloaded from GitHub, Node.js runtimes from nodejs.org, toolchains from nodejs.org and go.dev (all need network), and `actions/checkout` of the workflow's own repo copies the local working tree. Flags go before the workflow path. Before each job, what it needs is scanned and installed (`-install`): `toolchains` (the default) only writes into the user cache dir; `all` also installs apt packages with sudo, so it is meant for machines Raccoon CI owns, not a developer's laptop.

`raccoonci serve (-repo owner/name [-repo …] | -org name) [-label-prefix raccoon-] [-template ci] [-warm N] [-idle N] [-second-value 0.01] [-max-jobs 8] [-usage-file f]` runs the GitHub-triggered path: it polls the repositories for queued jobs whose `runs-on` label starts with the prefix and runs each on an InstaBox machine made from the `ci` template. The GitHub token comes from `GITHUB_TOKEN` (repo scope; with `-org` also `admin:org`), the InstaBox key from `INSTABOX_API_KEY`/`IBX_API_KEY`; neither is ever printed or put on a command line. The implementation is lib's `pkg/ghrunner` (see its notes in lib's CLAUDE.md); the keep-alive research is in `docs/research/idle-runners/` here.

Catalog flags go before the command: `-catalog extra.yaml` overlays a local file, `-catalog-url https://…` (env `RACCOONCI_CATALOG_URL`) pulls a feed, and `-catalog-key` (env `RACCOONCI_CATALOG_KEY`; several public keys comma-separated, any one that verifies is enough) requires an ed25519 signature. `raccoonci catalog update` refreshes the feed now. `DefaultFeedURL` and `TrustedKeys` in `pkg/catalog/keys.go` are the defaults used when a URL / key is not given (`catalog.ResolveFeed`). `DefaultFeedURL` is the feed at `https://raw.githubusercontent.com/Raccoon-CI/catalog/main/catalog.yaml` (the maintainer's `Raccoon-CI/catalog` repository, published and verified on 2026-11-03), so a bare `raccoonci catalog update` pulls it and requires the signature under `TrustedKeys`. The files that repository holds (`sign.sh`, the `publish.yml` workflow, a starter `catalog.yaml`, READMEs) are kept in `deploy/catalog-feed/`; `TrustedKeys` has the maintainer's public key (its private half is only in the publishing repository's Actions secrets), so a feed pulled with `-catalog-url` and no `-catalog-key` must be signed by it. For the maintainer: `raccoonci catalog keygen -out FILE` makes a key (private key to FILE, 0600; only the public key is printed), `raccoonci catalog sign FILE` writes FILE.sig with the private key read only from the environment variable `RACCOONCI_CATALOG_SIGNING_KEY` (never an argument). The server (backend repository, `internal/server/catalogsync`) pulls the same feed with `-catalog-url`/`-catalog-key`/`-catalog-interval`, keeps the verified text in the `documents` table, plans with built-in + feed (`Executor.Catalog`) and sends the text to the machine in `InstanceSpec.Catalog` (a string), which verifies and overlays it.

## Architecture

Pipeline (the parts after `plan.Build` live in the runner and lib repositories): `workflow.Parse` (lib) → `plan.Build` (evaluates what's knowable before the run, expands matrices, scans each instance's needs) → `engine.Run` (runner repository: `jobgraph` plus the local executor; per instance it scans, provisions, then calls) → `runner.Run` (one job instance). See `TODO.md` for the full picture.

- `internal/plan`: expands each job into `Instance`s (one per matrix combination), evaluating `runs-on` with `matrix`/`strategy` contexts. `Instance.Unavailable()` flags OSes the catalog says aren't available (`UnavailableMsg` carries the text). `pkg/platform` just reads `runs-on` values and asks the catalog. Product rule: those runs fail with a clear message or go to a user-connected machine. They are never skipped silently and never moved to Linux.
- `internal/envsnap`: when a snapshot of a prepared environment (dependencies installed) can be reused. `Compute` hashes the lockfiles (`toolchains.<name>.lockfiles` in the catalog) of the toolchains a job uses, plus the machine image's identity, into a key. Every project of a used toolchain counts, in the root or below it (a project is a directory with a manifest or lockfile of that toolchain; `.git`, `node_modules`, `vendor` and the like are skipped, the walk is capped): each lockfile goes in under its path from the root, so a change in `backend/go.sum` gives a new key. A manifest with no lockfile in the same directory makes the project unsnapshottable (`ErrUnpinned`: nothing says when its dependencies change), even when a lockfile sits in an ancestor directory (nothing says the manifest is a workspace member). The key covers all projects of the toolchain, not only those the job works in, which can rebuild a snapshot more often than needed but never reuses a stale one. A toolchain with no lockfile anywhere and no manifest needs nothing. Toolchain *versions* are deliberately not in the key: toolchains come with the machine or are installed on demand, the snapshot is for what they install. `Index` is the file that maps keys to the provider's snapshot ids, holds at most N and returns the least recently used (and replaced) ones for the caller to delete. `raccoonci plan` prints per instance whether it could be snapshotted. `raccoonci run -on instabox -env-snapshots N` wires it in (`cmd/raccoonci/remote.go`, `instabox.RunOptions`/`Client.Snapshot`): a hit starts the machine from the snapshot (if it is gone, 400/404, the entry is dropped and the instance reruns from the runner snapshot); a miss lets the first successful run per key clean its machine, snapshot it in the background and then delete it, and evicted entries are deleted through the API. Verified on InstaBox with a small Go project: miss and save, hit (build 4.2 s to 0.3 s), changed go.sum saves a new snapshot and evicts the old one. Rules, in the package comment: snapshot only from trusted refs (never a fork PR, whose install scripts could poison every later run with the same lockfile) and only from a machine with no secrets on its disks, since a snapshot copies sign-ins too. Measured on InstaBox (cli/cli): restore 6 to 12 s plus a 2.9 s warm build, against ~39 s cold; see the memory notes before assuming it pays for small projects.
- `sched` (decides which job instances share a machine) and `ghrunner` (the GitHub-triggered path behind `raccoonci serve`) are packages of the lib repository now (`pkg/sched`, `pkg/ghrunner`); their architecture notes are in lib's CLAUDE.md. This repository keeps the commands that use them (`cmd/raccoonci/serve.go`, `webhooks.go`, `remote.go`).
- Benchmark: lives in its own repository, https://github.com/Raccoon-CI/raccoonbench (it builds `raccoonci` from a checkout and compares it with older versions of itself, GitHub's hosted runners and InstaBox's runner). It reads `raccoonci run -usage-file` output (`instabox.Usage` as JSON) and the `results ready after N s` and per-job summary lines of `raccoonci run`, so changing those formats means changing it too.
- Paused spares (`pkg/instabox/spares.go` in lib, `run -on instabox -warm N`): a machine from the runner snapshot takes 6 to 20 s to create, but one paused with its agent running resumes in under a second (measured: create 5.6 s, suspend 3.1 s, resume 0.7 s with `processesPreserved`). `Spares.Claim` lists machines whose metadata says `raccoonci=warm` and the current runner snapshot, claims one and resumes it, then marks it `in-use`. Resuming cannot be the claim (resuming a machine that already runs succeeds, which handed one spare to two concurrent jobs in a real run: both wrote slot 0 and one never got its config), so a claim is a set in this process plus a token written into the machine's metadata and read back across processes; `Provider.obtain` tries it before creating, for the runner snapshot only (an environment-snapshot machine is created). `Spares.TopUp`, at the end of a run (after the answer is out), deletes spares made from another snapshot, and creates the missing ones, waits for the agent service (`systemctl is-active`), suspends them and stamps the pause time in the metadata, so they outlive the process. Used machines are deleted, never paused again: what a job left on one must not reach the next. Spares have the API's longest server-side deadline (24 h). `Usage` marks them (`Spare` for the running part of making one, `Resumed` and `PausedSec` for the one that was claimed); a paused spare is priced at the storage rate only, an assumption to check against a real bill.
