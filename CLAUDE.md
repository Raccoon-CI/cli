# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Racoon CI is a self-hosted CI/CD platform. Users move their workflows from GitHub Actions, Travis, CircleCI or Azure Pipelines onto compute they choose.

- The MVP config format is GitHub Actions workflow YAML. Migrating from GitHub must be seamless and must not lose GHA features. A native config format may come later.
- Compute providers are pluggable. Priority: the user's own machines, then InstaBox (first sandbox target), then Boat.dev, then cheap clouds (DigitalOcean/Linode). AWS/GCP are out of scope. Runners are Linux only for now, and InstaBox machines run Debian 13 (trixie), not Ubuntu, even for jobs that say `ubuntu-latest`. Windows may come later on InstaBox; macOS is too costly to host, so it is only reachable through users' own machines. The parser still accepts `macos-*`/`windows-*` labels, because migrated workflows contain them.
- Racoon CI runs workflows with its own engine; it is not a GitHub self-hosted runner orchestrator (the user rejects paying two bills). It must reimplement the runtime services official actions call (artifact, cache, OIDC, job summary). See `docs/gha-compat.md` for what act and Gitea already do.
- Planned: AI agents that triage or fix failed runs.

The code is a Go skeleton (module path `racoonci`). The Makefile and layout follow the sibling project `../release-manager`.

## Commands

```sh
make build                # builds binaries into dist/ (VERSION=x.y.z sets main.version)
make test                 # go test ./...
make race                 # go test -race ./...
make vet                  # go vet ./...
make check                # vet + race — must pass before merging
go test ./internal/foo -run TestName -v   # run a single test
make runner-images        # refresh the embedded snapshot of GitHub's Ubuntu runner images (network)
```

## Layout

- `cmd/<binary>/main.go`: one directory per executable. Keep `main` thin: parse flags, wire dependencies, call into `internal/`.
- `internal/<pkg>/`: all application logic goes here (not importable from outside the module).
- `version` lives in `package main` and is set through `-ldflags "-X main.version=..."` in the Makefile.

New binaries also need to be added to the `build` target in the Makefile.

`racoonci plan <workflow.yml>` parses a workflow and prints its jobs, matrix combinations, each run's `runs-on`, what each run needs installed, and warnings for OSes we can't provide. Use it to sanity-check the parser, evaluator and scanner against real workflow files.

`racoonci run [-job id] [-secret NAME] [-var K=V] [-workdir dir] <workflow.yml>` runs a workflow on this machine. Actions are downloaded from GitHub, Node.js runtimes from nodejs.org (both need network), and `actions/checkout` of the workflow's own repo copies the local working tree. Flags go before the workflow path.

Catalog flags go before the command: `-catalog extra.yaml` overlays a local file, `-catalog-url https://…` (env `RACOONCI_CATALOG_URL`) pulls a feed, and `-catalog-key` (env `RACOONCI_CATALOG_KEY`) requires its ed25519 signature. `racoonci catalog update` refreshes the feed now.

## Architecture

Pipeline: `workflow.Parse` → `plan.Build` (evaluates what's knowable before the run, expands matrices, scans each instance's needs) → `engine.Run` (job graph) → `runner.Run` (one job instance). Still to come: the provisioner (installing scanned demands), the runtime-service servers (artifacts, cache), and the InstaBox provider.

**Knowledge that changes as tools evolve belongs in `internal/catalog/catalog.yaml`, not in Go code.** This covers:
- which commands belong to which toolchain, tool or apt package
- setup actions and their inputs
- version-file formats
- where the runner-images report states the defaults
- which Node.js major each `runs.using` runs on
- runner labels to OS (`ubuntu-*` → linux), which OSes are available, and the tracked runner images with their labels and toolset files
- the machine's own platform (Debian 13 / trixie) and the Ubuntu-to-Debian warnings (`lsb_release`, PPAs)
- how wrapper commands (`sudo`, `env`, `timeout`) and package installers (`apt-get install`) are read

Code holds only the mechanisms: parsers for the file formats it names, the report walker, the shell parser. When adding support for a tool, edit the YAML. A Go change is only needed for a new *kind* of rule.

What stays in code on purpose is GitHub's own protocol, which changes with GitHub's releases, not with the tool ecosystem: workflow and `action.yml` keys, workflow command names, expression syntax, shell command lines, and limits such as 256 matrix jobs. Download locations (GitHub, nodejs.org) also stay in code, because the catalog may come from the network and must not be able to redirect downloads.

The catalog is layered: built-in, then a feed pulled from `-catalog-url` (cached on disk, refreshed daily with ETag, optional ed25519 signature), then the local file. Because it may come from the network, a catalog only *names* things: every name is checked against a strict character set, and it cannot hold commands or URLs. A failed pull never stops a run: it falls back to the cache and then to the built-in one, and says why on stderr.

- `internal/value`: the dynamic data model shared by the parser and the expression evaluator. Values are nil/bool/float64/string/[]any/`value.Object`. `Object` is an *ordered* key list, because matrix expansion order and GitHub's generated job names depend on key order. All numbers are float64, matching GitHub (so `3.10` becomes `3.1`, as it does there). `value.Decoder` walks `yaml.Node` trees with a shared node budget, which guards against alias bombs in untrusted PR workflow files. Use one Decoder per document.
- `internal/workflow`: `Parse` turns workflow YAML into `Workflow`/`Job`/`Step`. Fields that may be literals or `${{ }}` expressions stay as raw `any` values and are evaluated later. Like GitHub, it rejects unknown keys, missing `runs-on`/`steps`, bad needs and cycles. `ExpandMatrix` implements GitHub's include/exclude rules and takes an already-evaluated matrix; dynamic matrices (`${{ fromJSON(...) }}`) must go through the evaluator first. The matrix tests encode the examples from GitHub's docs, so keep them passing.
- `internal/expr`: a hand-written lexer, parser and evaluator for `${{ }}`, following GitHub's documented semantics: mismatched types compare as numbers, strings compare case-insensitively, `&&`/`||` return operands, `*` object filters, and objects/arrays are equal only to the same instance. `Interpolate` keeps the type of a lone `${{ x }}` (so `false` stays falsy and `fromJSON` can build a matrix) and stringifies mixed text. `EvalCondition` adds the implicit `success() &&` and treats mixed text as an always-true string, as GitHub does. A missing context returns `*expr.UnknownContextError`, which `plan` uses to defer values that are only known at run time.
- `internal/plan`: expands each job into `Instance`s (one per matrix combination), evaluating `runs-on` with `matrix`/`strategy` contexts. `Instance.Unavailable()` flags OSes the catalog says aren't available (`UnavailableMsg` carries the text). `internal/platform` just reads `runs-on` values and asks the catalog. Product rule: those runs fail with a clear message or go to a user-connected machine. They are never skipped silently and never moved to Linux.
- `internal/runnerimages`: what GitHub's hosted Ubuntu images preinstall (default tool versions, toolcache versions, apt packages, CLIs), so jobs that rely on preinstalled tools get the same versions here. `Fetcher.Latest` reads the *fully deployed* release from runner-images' README status badge (a gist, not the API: no token, no 60/hour limit). It falls back to the releases API (newest non-prerelease) when the badge is missing, malformed, or names a version whose report can't be downloaded. Data per release comes from the `internal.<image>.json` release asset, plus the toolset at the same tag for Go's default. Which images exist, their labels and toolset file names come from the catalog, and `ubuntu-latest` is resolved from the README table (`Snapshot.Aliases`, written to `aliases.json`). `Snapshot.ForLabel(cat, label)` needs the catalog. The embedded `snapshot/` is the offline fallback. The planned server will poll the same sources into its database. Only send the token to api.github.com.
- `internal/catalog`: loads and validates `catalog.yaml`. The file is embedded, and `Overlay` applies another catalog on top: same-name entries replace, `name: null` removes, top-level lists replace. Validation rejects:
  - unknown keys
  - a command claimed by two entries
  - references to undefined toolchains
  - unknown version-file formats
  - names with characters that could inject options or paths (`-o…`, `../`)
  - bad regexps and unknown message placeholders

  A catalog with a higher `schema` than the code understands is refused as a whole (`ErrNewerSchema`). `TestDefaultIsValid` guards the embedded file. `Source` (`source.go`) pulls a feed:
  - https only, no redirect downgrade
  - size-capped
  - conditional requests with ETag
  - the cache is rewritten only after the new file has been verified and validated
  - cached content is re-verified on every read, so a tampered cache file is ignored
- `internal/scan`: a static, best-effort scan of what a job instance needs installed. It never fails a job.
  - Explicit demands are `setup-*` versions, from inputs, matrix values, or version files like `.nvmrc`/`go.mod`/`.tool-versions`.
  - Implicit demands are commands found in `run:` scripts that the GitHub image preinstalls and that no earlier step set up. Scripts are parsed with `mvdan.cc/sh`, not regex. Implicit demands use the image's default versions from `runnerimages`, and are only reported when that image really has the tool.
  - What commands, actions and files mean comes from the catalog.
  - Opaque build commands (`make`, `./x.sh`) plus a repo manifest (`go.mod`, `package.json`, ...) imply that toolchain.
  - It warns about the catalog's `compat-warnings` (Ubuntu-only patterns such as `lsb_release` and PPAs) and YAML-float versions (`3.10` → `3.1`, with the affected versions listed per toolchain in the catalog).
  - Container jobs only need docker.
  - Repo reads are size-capped. Pass an `os.OpenRoot` FS (as `racoonci plan` does) so symlinks can't escape the repo.
- `internal/runner`: runs one job instance the way GitHub's runner does.
  - `run:` steps use GitHub's exact shell command lines. Unspecified shell is `bash -e`, explicit `bash` adds `--noprofile --norc -o pipefail`, and `python` falls back to `python3` on Debian.
  - Step conditions use the job's status. Composite actions have their own steps, inputs and `success()`.
  - Supports `continue-on-error` (outcome vs conclusion) and step and job timeouts. Cancellation sends SIGINT, then SIGKILL to the step's process group. `always()` and post steps get a 5-minute grace period.
  - Workflow commands are supported in both `::x::` and legacy `##[x]` forms, and so are the file commands (`$GITHUB_ENV`/`OUTPUT`/`PATH`/`STATE`/`STEP_SUMMARY`).
  - Secrets and `add-mask` values are masked in every event.
  - JavaScript actions support pre, main and post, `INPUT_*`/`STATE_*`, and post-if defaulting to `always()`. Composite actions are supported too.
  - `set-env`/`add-path` are disabled unless `ACTIONS_ALLOW_UNSECURE_COMMANDS` is set, and `NODE_OPTIONS` is refused via `$GITHUB_ENV`, both as on GitHub.
  - A step ends when its shell exits, even if background processes keep the pipes open (`server &` works). Leftover processes are killed at job end.
  - Not yet supported: container/service jobs, Docker actions, problem matchers.
- `internal/engine`: runs the job graph.
  - Jobs wait for their `needs`. Job `if:` uses `expr.Status`, where `Skipped` means a need didn't succeed. `failure()` is transitive over ancestors.
  - Matrices are expanded at run time, so `fromJSON(needs...)` works.
  - Supports fail-fast, `max-parallel`, and job-level `continue-on-error`.
  - Unavailable OSes fail that instance with `platform.Unavailable`, per the product rule.
  - Reusable workflows are not supported yet.
- `internal/actions`: parses `action.yml`. Unknown keys are ignored, as GitHub does. It also downloads actions: through codeload without a token, through the API with one, and once per `Store`, because tags move.
- `internal/archive`: tar.gz extraction through `os.Root`. Entries can't escape the destination via `..`, absolute paths, symlinks or hard links.
- `internal/nodejs`: downloads the Node.js runtimes for JavaScript actions from nodejs.org and checks them against SHASUMS256. When offline, it uses an installed version.
- `internal/local`: support code for `racoonci run`.
  - Builds the github context from git.
  - Implements checkout by copying the working tree.
  - Renders the terminal output.
- Deliberately not depending on act's `pkg/model` (it pulls in go-git/logrus and silently drops dynamic matrices), on actionlint's AST (it stringifies all scalars, so `false` would become truthy), or on actionlint at all (12 direct deps for a ~500-line parser). act and actionlint are fine as reference implementations.
