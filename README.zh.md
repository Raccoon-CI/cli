# raccoonci（cli）

[English](README.md)

`raccoonci` 是 Raccoon CI 面向用户的命令。Raccoon CI 是自托管的 CI/CD 平台，把 GitHub Actions workflow 跑在你选的算力上。

```sh
raccoonci plan .github/workflows/ci.yml     # 列出 job、matrix 组合、每次运行需要装什么
raccoonci match -h                          # 某个事件会不会触发该 workflow 的 on:？
raccoonci affected -h                       # 改动的文件涉及哪些 job？
raccoonci run .github/workflows/ci.yml      # 在本机运行 workflow
raccoonci run -on instabox …                # 在新建的 InstaBox 机器上运行
raccoonci serve -repo owner/name            # GitHub 触发路径：带 raccoon-* 标签的 job 跑在 InstaBox 上
raccoonci catalog update                    # 拉取并验签 catalog feed
```

所有命令见 `raccoonci -h`。

## 与其他仓库的关系

| 仓库 | 内容 |
|---|---|
| **cli**（本仓库） | `raccoonci`，用户运行的命令：`plan`、`match`、`affected`、`run`、`serve`、`catalog` 等 |
| runner | `raccoonci-runner`（机器侧二进制），以及本命令调用的引擎（公开包 `actions`、`cliflag`、`cliutil`、`engine`、`runner`、`runnercmd`、`scan`） |
| backend | `raccoonci-server`（服务端） |
| lib | 共用的包（`pkg/*`）：workflow 解析、表达式、catalog、wire 协议、InstaBox 客户端等 |

依赖只有一个方向：lib ← runner ← cli，以及 lib ← backend。本仓库只引用 runner 和 lib 的公开包，不引用它们的 `internal/`。

## 构建与测试

```sh
make build        # dist/raccoonci
make check        # go vet + go test -race
```

需要 Go 1.27 或更新版本。模块依赖 `lib v0.1.0` 和 `runner v0.1.0`（都已发布），所以直接 `go build ./cmd/raccoonci` 就行。同时改几个仓库时见 `CLAUDE.md` 的 "Developing across the repositories"。

## 进度

已完成和未完成的事项见 `TODO.md`。

## 许可证

Apache-2.0，见 `LICENSE`。第三方声明见 `THIRD_PARTY_NOTICES`。
