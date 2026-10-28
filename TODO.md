# Racoon CI 进度与待办

更新于 2026-10-28。代码约 8400 行（不含测试），89 个测试函数，`make check` 全绿。

图例：✅ 完成　🟡 部分完成　⬜ 未开始

## 一、MVP 总览

按已定的顺序：

| # | 阶段 | 状态 | 说明 |
|---|------|------|------|
| 1 | workflow 解析 + matrix 展开 | ✅ | 含表达式求值 |
| 2 | 依赖扫描器 | ✅ | 只"报告"，还没人用它去装东西（见 3.1） |
| 3 | 单机执行器 | 🟡 | `run:`、JS/composite action 可用；容器、Docker action、可复用 workflow 不支持 |
| 4 | artifact v4 + cache v1 服务 | ⬜ | 实测 `upload-artifact` 报错，`cache` 静默不命中 |
| 5 | InstaBox provider | ⬜ | |

在 3 和 4 之间插入的"装环境"（按扫描结果安装依赖）也还没做，它是目前最大的缺口。

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

**真实环境实测过的**：`actions/setup-node@v4` 真实下载并运行；`runner-images update` 联网成功。其余网络相关代码只有假服务器测试。

**文档**：`CLAUDE.md`（架构与约定）、`docs/gha-compat.md`（GHA 运行时依赖调研，参考 act / Gitea）

## 三、未完成

### 3.1 装环境（provisioner）——最大缺口，下一步

扫描器能算出"这个 job 需要 node 22、jq、go 1.24"，但 `racoonci run` 完全没用这个结果，缺什么就直接失败。

- ⬜ 并行下载工具链，放进 toolcache 目录结构（`<tool>/<ver>/<arch>` + `.complete`），让 `setup-*` 直接命中
- ⬜ apt 包安装，并用 catalog 里已有的 `debian:` 字段做 Ubuntu→Debian 包名映射（字段已有，代码还没用）
- ⬜ Python：`setup-python` 下载的预编译包是为 Ubuntu 构建的，在 Debian 上能不能用**还没验证**；备选 python-build-standalone
- ⬜ 第三方 action 内部的依赖：composite/docker action 要下载 `action.yml` 才能扫描（现在只扫 workflow 本身）
- ⬜ 运行时兜底：命令返回 127（command not found）时记录并提示，下次自动补装
- ⬜ 下载镜像配置（国内访问 GitHub / nodejs.org 慢）。用本地配置或环境变量，**不放进 catalog feed**

### 3.2 运行时服务（GHA action 依赖的内部接口）

- ⬜ artifact v4（Twirp）与 v3 REST。**实测**：`upload-artifact@v4` 报 `Unable to get the ACTIONS_RUNTIME_TOKEN`
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
- ⬜ 用 Racoon CI 自己跑自己的 `make check`（dogfood），同时也是第一个真实的端到端用例
- ⬜ 兼容性语料：act 和 toolkit 的 workflow 文件**只验证了能解析**，整套跑起来的只有手写的小例子
- 🟡 硬编码原则：evolving 的名单都已进 catalog；GitHub 协议本身（字段名、workflow 命令名、上限）和下载地址有意留在代码里，原因见 `CLAUDE.md`
