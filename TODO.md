# Raccoon CI 进度与待办

更新于 2026-10-28。`make check` 全绿。

图例：✅ 完成　🟡 部分完成　⬜ 未开始

## 一、MVP 总览

按已定的顺序：

| # | 阶段 | 状态 | 说明 |
|---|------|------|------|
| 1 | workflow 解析 + matrix 展开 | ✅ | 含表达式求值 |
| 2 | 依赖扫描器 | ✅ | |
| 3 | 单机执行器 | 🟡 | `run:`、JS/composite action 可用；容器、Docker action、可复用 workflow 不支持 |
| 3.5 | 装环境（按扫描结果安装） | 🟡 | Node.js、Go、Python、apt 包已通；Java、Ruby、Rust 和各种 CLI 工具还没有安装器（见 3.1） |
| 4 | artifact v4 + cache v1 服务 | 🟡 | artifact v4 已通（本机运行，真实 action 实测）；cache v1 未做，`cache` 仍静默不命中 |
| 5 | InstaBox provider | ⬜ | |

## 二、已完成

**解析与求值**
- ✅ workflow 严格解析：拒绝未知字段、重复 key、环依赖，带行号；防 YAML alias 炸弹（`internal/workflow`、`internal/value`）
- ✅ matrix：include/exclude 规则按 GitHub 文档示例实现；256 job 上限；运行时展开（`fromJSON(needs…)` 动态 matrix）
- ✅ 表达式 `${{ }}`：词法/语法/求值，GitHub 的类型转换、大小写不敏感比较、`&&`/`||` 返回操作数、`*` 过滤、全部内置函数（`internal/expr`）
- ✅ `if:` 条件：隐式 `success()`、`failure()` 对祖先 job 传递、job 级 skipped 传播

**环境知识**
- ✅ `runnerimages`：GitHub 官方镜像预装了什么（默认版本、缓存版本、apt 包、CLI）。优先读 README 状态 badge（不占 API 额度），失败回退到 releases API；内置 3 个镜像的快照作离线兜底
- ✅ `catalog`：把"命令属于哪个工具链、setup action、版本文件、runner 标签、镜像、Ubuntu→Debian 警告"等全部做成数据（`catalog.yaml`），内置默认值
- ✅ catalog 拉取：`-catalog-url` 拉 feed，ETag + 每日刷新，离线用缓存，坏内容不覆盖好缓存，可选 ed25519 签名，只允许 https，名字做字符集校验防注入
- ✅ 扫描器 `scan`：`setup-*` 版本（输入/matrix/版本文件）、`run:` 脚本里没声明的命令（用 shell 解析器）、不透明构建命令（`make` 等）结合仓库清单文件推断、Ubuntu 专属写法警告、YAML 浮点版本（`3.10`→`3.1`）警告
- ✅ `plan` 命令：列出 job、matrix 组合、每个组合要装什么，并提前警告不可用的 OS

**执行**
- ✅ `runner`：`run:` 步骤（GitHub 同款 shell 命令行）、`continue-on-error`、步骤/job 超时、取消（SIGINT→SIGKILL，`always()` 与 post 步骤有 5 分钟宽限）
- ✅ workflow 命令（新旧两种格式）和文件命令（`$GITHUB_ENV/OUTPUT/PATH/STATE/STEP_SUMMARY`）；密钥在日志中打码
- ✅ JS action（pre/main/post、`INPUT_*`、`STATE_*`）、composite action；Node 运行时从 nodejs.org 下载并校验 SHA256
- ✅ `engine`：job 依赖图、job 级 `if`、fail-fast、`max-parallel`、job 级 `continue-on-error`；不可用 OS 的实例明确失败，其余照常（不静默跳过、不回退 Linux）
- ✅ 安全：下载的压缩包无法写出目标目录；仓库读取限制在仓库内并限大小
- ✅ CLI：`plan`、`run`、`catalog update`、`runner-images update`

**装环境**（`internal/provision`、`internal/semver`）
- ✅ 每个 job 启动前自动扫描并安装。作业用了 `node` 却没声明，就装镜像默认版本并放到 PATH 最前；声明了 `setup-node` 的，把 toolcache 预先填好，官方 action 直接命中缓存、不再下载
- ✅ 安装器：Node.js（nodejs.org，支持 `lts/*`、`lts/iron`、`lts/-1`、`latest`）、Go（go.dev，支持 `stable`/`oldstable`）和 Python（python-build-standalone），下载校验 SHA-256，解压到私有目录再原子改名；并发的 job 共用一次下载；中断留下的半成品会被替换
- ✅ **Python 在 Debian 上能用了**：实测 `setup-python` 在干净的 Debian 13 上直接失败（GitHub 的 Python 构建是给 Ubuntu 的）；把 Python 预先放进 toolcache 后它直接命中，`Successfully set up CPython (3.12.16)`。隐式用到的 `python3`（镜像默认 3.12.3，已不再发布）放宽到同一小版本的最新补丁；workflow 里显式写死的版本绝不偷换，而是给出明确提示
- ✅ 版本规格：`20`、`20.x`、`>=18`、`^1.22`、`~1.2.3`、`1.2 - 1.4`、`||`；缓存里已有满足的版本就不联网（离线可用）
- ✅ apt：`-install=all` 时用 sudo 安装缺少的包，Ubuntu→Debian 包名用 catalog 的 `debian:` 字段；一个包装不上不影响其余；默认不装，只报告
- ✅ 装环境失败只给警告，不会让作业失败（扫描结果只是启发式）

**真实环境实测过的**：`actions/setup-node@v4` 真实运行；`runner-images update` 联网成功；`setup-node` 输出 `Found in cache`，说明预填被官方 action 认可。在一台**全新的 InstaBox Debian 13 沙箱**（没有 go、make、gcc、tree、pip）上，一个没声明任何版本的 job 用 5 秒装好 node 22.23.3、go 1.24.13、make、gcc、tree 并通过；`setup-python@v5` 修复后通过。其余网络相关代码只有假服务器测试。

**测试机**：为此建了一台 InstaBox 沙箱 `test-machine`（`m_EXAMPLE_MACHINE_ID`，已停止）。MCP 没有删除工具，**需要在控制台手动删除**。

**文档**：`CLAUDE.md`（架构与约定）、`docs/gha-compat.md`（GHA 运行时依赖调研，参考 act / Gitea）

## 三、未完成

### 3.1 装环境——剩余部分

Node.js、Go、apt 包已经通了（见"已完成"）。还缺：

- 🟡 Python：最新一期 python-build-standalone 只含每个小版本的最新补丁，且已不含 3.10 及更早。工作流若显式写 `3.10` 或某个旧补丁，现在会得到明确报错而不是安装；要支持就得去翻更早的发布（要用 GitHub API，有 60 次/小时的限制）
- ⬜ Java（Temurin）、Ruby、Rust 的安装器
- ⬜ 各种 CLI 工具的安装器：docker、gh、kubectl、helm、terraform、yarn、maven 等。目前只检查是否已在 PATH 上，缺了就警告
- ⬜ 其他架构：toolcache 的 arch 目录已按机器架构命名，但只在 x64 上实测过
- ⬜ 第三方 action 内部的依赖：composite/docker action 要下载 `action.yml` 才能扫描（现在只扫 workflow 本身）
- ⬜ 运行时兜底：命令返回 127（command not found）时记录并提示，下次自动补装
- ⬜ 下载镜像配置（国内访问 GitHub / nodejs.org / go.dev 慢）。用本地配置或环境变量，**不放进 catalog feed**
- ⬜ 工具链缓存清理（旧版本只增不减）。实测一套 node/go/python 就有 1GB，在 InstaBox 的 `/tmp`（2GB tmpfs）上会把盘占满，工具缓存应放在 `/data` 之类的持久盘上，并设上限

### 3.2 运行时服务（GHA action 依赖的内部接口）

- ✅ artifact v4（Twirp + Azure Blob 的 Put Block / Put Block List）：`internal/artifact`、`internal/blob`（本地目录和 S3 兼容存储）。`raccoonci run` 里 `upload-artifact@v4` 与 `download-artifact@v4` 跨 job 实测通过（20 MB，sha256 一致）
- ⬜ artifact 剩余：v3 REST；按 job 签发 token（现在整个 run 共用一个）；元数据持久化（现在在内存）；retention 到期清理（`expires_at` 只存不执行）；整体 sha256 校验（现在只记录）；服务端接 S3 并在真实 AWS 上验证（目前只测过 SeaweedFS）；`download-artifact` 的按 ID 跨 run 下载（需要 GitHub token，未做）
- ⬜ cache v1（`/_apis/artifactcache`）。**实测**：`actions/cache@v4` 只打一条 warning 就当未命中，job 还显示成功，等于缓存没生效且不易察觉
- ⬜ OIDC token 端点（`ACTIONS_ID_TOKEN_REQUEST_URL`）
- ⬜ 真实的 `GITHUB_TOKEN`（GitHub App installation token，调用真实 GitHub API）
- ⬜ job summary 的存储与展示（现在只收集在内存里）、annotations 持久化

### 3.3 job 类型缺口（现在会给出明确报错，不会静默）

- ⬜ 容器 job（`container:`）和 `services:`
- ⬜ Docker action（`uses: docker://…`、`runs.using: docker`）
- ⬜ 可复用 workflow（`jobs.<id>.uses`、`workflow_call`）
- ⬜ problem matcher（目前输出原样记录，不生成 annotation）

### 3.4 workflow 语义缺口

- ⬜ `on:` 触发匹配：branches/paths/tags 过滤、`schedule`、`workflow_dispatch` 输入校验、`workflow_call`。现在 `run` 只是假设事件发生了
- ⬜ `concurrency`（group / cancel-in-progress）
- ⬜ `permissions` 和 `environment`（含保护规则、环境级 secrets）
- ⬜ `run-name`
- 🟡 `github` 上下文：`run` 命令从 git 生成了一部分字段，真实的 run_id、事件载荷等要服务端提供
- 🟡 `hashFiles`：自己写的 glob 实现，**没有逐项对照 `@actions/glob`**，边界行为可能有差异

### 3.5 服务端（完全没开始）

- ⬜ 事件入口（GitHub App / webhook）、按 sha 或 PR merge ref 拉代码
- ⬜ 调度与队列、数据库（runs、jobs、日志、runner-images 与 catalog 数据）
- ⬜ API、UI、日志流、把状态回写 GitHub（checks / statuses）
- ⬜ secrets / vars 的加密存储
- ⬜ 工作目录清理与磁盘配额（现在 workdir 只增不减）
- ⬜ 服务端定期轮询 runner-images（ETag + 存库），目前只有命令行手动更新

### 3.6 算力提供方（provider）

优先级：用户自己的机器 → InstaBox → Boat.dev → DigitalOcean/Linode。

- ⬜ 用户自己的机器（agent 连接）
- ⬜ InstaBox（Debian 13）
- ⬜ Boat.dev
- ⬜ DigitalOcean / Linode
- ⬜ 不可用 OS 的兜底：现在只有"明确失败 + plan 提前警告"；"连接自己的 Mac / Windows 机器去跑"还没做
- ⬜ Windows 支持（可能以后放在 InstaBox 上）；macOS 只能走用户自己的机器

### 3.7 安全与隔离

- ⬜ 沙箱：job 现在直接以当前用户在本机运行，隔离完全依赖 provider 的虚拟机
- ⬜ 来自 fork PR 的 workflow 的 secrets 策略
- ⬜ catalog feed：默认地址（还没发布，**不能编一个**）、签名生成流程和工具

### 3.8 MVP 之后

- ⬜ AI agent：失败 run 的分诊与自动修复（已提过的核心卖点）
- ⬜ 从 Travis / CircleCI / Azure Pipelines 迁移（MVP 只做 GitHub Actions 格式）
- ⬜ 原生配置格式

## 四、工程与风险

- ⬜ **git 提交**：上次提交（`59c4a82`）之后的 catalog 拉取与硬编码清理还没提交（工作区 20 项改动）
- ⬜ 用 Raccoon CI 自己跑自己的 `make check`（dogfood），同时也是第一个真实的端到端用例
- ⬜ 兼容性语料：act 和 toolkit 的 workflow 文件**只验证了能解析**，整套跑起来的只有手写的小例子
- 🟡 硬编码原则：evolving 的名单都已进 catalog；GitHub 协议本身（字段名、workflow 命令名、上限）和下载地址有意留在代码里，原因见 `CLAUDE.md`
