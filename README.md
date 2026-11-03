# raccoonci (cli)

[中文](README.zh.md)

`raccoonci` is the user-facing command of Raccoon CI, a self-hosted CI/CD platform that runs GitHub Actions workflows on compute you choose.

```sh
raccoonci plan .github/workflows/ci.yml     # jobs, matrix combinations, what each run needs installed
raccoonci match -h                          # does an event trigger this workflow's on:?
raccoonci affected -h                       # which jobs do the changed files concern?
raccoonci run .github/workflows/ci.yml      # run a workflow on this machine
raccoonci run -on instabox …                # run it on a fresh InstaBox machine
raccoonci serve -repo owner/name            # GitHub-triggered path: jobs labelled raccoon-* run on InstaBox
raccoonci catalog update                    # pull and verify the signed catalog feed
```

Run `raccoonci -h` for all commands.

## How it relates to the other repositories

| repository | what it holds |
|---|---|
| **cli** (this one) | `raccoonci`, the command users run: `plan`, `match`, `affected`, `run`, `serve`, `catalog`, … |
| runner | `raccoonci-runner`, the machine-side binary, and the engine this command calls (public packages `actions`, `cliflag`, `cliutil`, `engine`, `runner`, `runnercmd`, `scan`) |
| backend | `raccoonci-server`, the server |
| lib | shared packages (`pkg/*`): workflow parser, expressions, catalog, wire protocol, InstaBox client, … |

Dependencies point one way: lib ← runner ← cli, and lib ← backend. This repository imports only the public packages of runner and lib, never their `internal/`.

## Build and test

```sh
make build        # dist/raccoonci
make check        # go vet + go test -race
```

Go 1.27 or newer. The module requires `lib v0.4.0` and `runner v0.2.0` (both published), so a plain `go build ./cmd/raccoonci` works. See `CLAUDE.md` ("Developing across the repositories") for working on several repositories at once.

## Status

See `TODO.md` (Chinese) for what is done and what is open.

## License

Apache-2.0, see `LICENSE`. Third-party notices: `THIRD_PARTY_NOTICES`.
