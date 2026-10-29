# Raccoon CI 进度与待办

更新于 2026-10-28。`make check` 全绿。

图例：✅ 完成　🟡 部分完成　⬜ 未开始

## 一、MVP 总览

按已定的顺序：

| # | 阶段 | 状态 | 说明 |
|---|------|------|------|
| 1 | workflow 解析 + matrix 展开 | ✅ | 含表达式求值 |
| 2 | 依赖扫描器 | ✅ | |
| 3 | 单机执行器 | 🟡 | `run:`、JS/composite action、容器 job 和 `services:` 可用；Docker action、可复用 workflow 不支持 |
| 3.5 | 装环境（按扫描结果安装） | 🟡 | Node.js、Go、Python、Java、Rust、apt 包已通；Ruby 和各种 CLI 工具还没有安装器（见 3.1） |
| 4 | artifact v4 + cache v1/v2 服务 | ✅ | 两者在本机运行里都用真实 action 实测通过；v3 REST、持久化/淘汰等见 3.2 |
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

**测试机**：为此建过一台 InstaBox 沙箱 `test-machine`（`m_EXAMPLE_MACHINE_ID`）。2026-10-28 核对时它已不在当前账号的机器列表里，无需再处理。

**文档**：`CLAUDE.md`（架构与约定）、`docs/gha-compat.md`（GHA 运行时依赖调研，参考 act / Gitea）

## 三、未完成

### 3.1 装环境——剩余部分

Node.js、Go、Python、Java、Rust、apt 包已经通了（见"已完成"和下面的条目）。还缺：

- 🟡 Python：最新一期 python-build-standalone 只含每个小版本的最新补丁，且已不含 3.10 及更早。工作流若显式写 `3.10` 或某个旧补丁，现在会得到明确报错而不是安装；要支持就得去翻更早的发布（要用 GitHub API，有 60 次/小时的限制）
- ✅ Java（Temurin，Adoptium 接口）和 Rust（static.rust-lang.org 的 rustc/cargo/rust-std/rustfmt/clippy 组件包，逐个校验 SHA-256 后用各自的 install.sh 装）的安装器（2026-10-29）。两者在 catalog 里标了 `implicit-only`：`setup-java`、rustup 自己会下载，预填 tool cache 它们不会读，所以只给“没写 setup 就直接用 `cargo`/`java`/`mvn`”的 job 装。真实网络实测：一个 job 隐式用 cargo、rustc、cargo fmt、java，11 s 装好。Rust 只支持 `stable` 和精确版本（1.80、1.80.0），Java 只支持主版本和精确版本（17、17.0.9、1.8），范围写法（`>=1.70`、`lts`）不支持；没有探测机器自带的 Rust 和 Java，总是下载
- ⬜ Ruby 的安装器
- ⬜ 各种 CLI 工具的安装器：docker、gh、kubectl、helm、terraform、yarn、maven 等。目前只检查是否已在 PATH 上，缺了就警告
- ✅ 采用机器自带的工具链：InstaBox 的 `universal` 模板自带 Go、Node 等。装环境前先按 catalog 的 `probe`（运行什么命令、从输出哪一段取版本号）探测，版本满足就把安装目录链接进 toolcache，不再下载。运行时探测，不写死模板里有什么。实测：全新缓存目录下跑自己的 CI，`go 1.27.1 (already on this machine)`，`setup-go` 直接 Found in cache。目前只给 node 和 go 配了 probe；Python 不采用系统版本（`setup-python` 要的布局不同），其余工具链（Rust、Java 等）还没有安装器也没有 probe
- 🟡 项目环境快照（装完依赖拍快照，锁文件不变就复用）：键和登记表已做（`internal/envsnap`）。键 = 锁文件内容哈希 + 用到的工具链名 + 机器镜像标识，不含工具链版本；`plan` 会显示每个实例能否快照。真实项目上：cobra/本项目（go.sum）、commander.js（package-lock.json）、click（uv.lock）可以；express 没有锁文件，判定为不能快照。**已接到 `-on instabox`（`-env-snapshots N`，默认关）**：实例启动前按键查登记表，命中就从该快照建机器，快照已被删（400/404）则忘掉并退回 runner 快照重跑；未命中时，成功的一次运行会在后台清理机器（删 agent 配置和本次 workspace，保留各种缓存）、拍快照、再删机器，同一个键同时只有一台机器拍；超出上限的旧快照由 API 删除。只在可信事件（非 `pull_request*`）、且没有 `-secret` 时才拍。**monorepo**：键只含仓库根的锁文件，所以用到的工具链在子目录里有清单或锁文件（跳过 `.git`、`node_modules`、`vendor` 等）时，判定为不能快照（`ErrUnpinned`），避免子目录依赖变了却复用旧快照；把子目录锁文件也算进键还没做。**还没做**：**真实 InstaBox 端到端已验证（2026-10-29）**：带 uuid 依赖的小 Go 项目，三次运行：①未命中，建机器 10.8 s，构建 4.2 s（下载依赖），成功后保存快照；②命中，机器 7.7 s，构建 0.3 s，整条命令 35 s → 10.6 s；③改 go.sum 后键变了，未命中并保存新快照，旧快照被淘汰（上限 1）。测试机器自动删除，测试快照和端口已手工清理
- ✅ 通用 runner 快照（InstaBox）：`universal` 模板（Debian 13）+ 静态编译的 `raccoonci`（`/usr/local/bin`）+ 清单 `/etc/raccoonci-runner.json`，不含工具链、依赖、机密、登录信息。做法：新建一台干净的 `universal`，临时用 `instabox-port serve` 发布只含二进制的目录（随机路径），机器上下载并校验 SHA-256 后立刻撤下端口，检查母机无凭据文件后拍快照。实测：从快照建机器约 9.5 s，在上面用快照里的 `raccoonci` 跑 checkout + `setup-go` + `setup-node` + 运行步骤，7.2 s 成功，Go 和 Node 都直接采用机器自带的版本。这次验证还发现并修了一个真问题：模板把 Go 和 Node 装在同一个 `/usr/local/bin`，采用时不能把整个前缀链接进某个工具链的目录，否则会把别的工具带上 PATH，盖掉前面步骤选的版本（已修，见 `sharesPrefix`）。**更新方式还是手工的**（每次新版本要重做快照）；要有自动化，并在快照清单里校验 `raccoonci` 版本和模板版本
- ⬜ 其他架构：toolcache 的 arch 目录已按机器架构命名，但只在 x64 上实测过
- ⬜ 第三方 action 内部的依赖：composite/docker action 要下载 `action.yml` 才能扫描（现在只扫 workflow 本身）
- ⬜ 运行时兜底：命令返回 127（command not found）时记录并提示，下次自动补装
- ⬜ 下载镜像配置（国内访问 GitHub / nodejs.org / go.dev 慢）。用本地配置或环境变量，**不放进 catalog feed**
- 🟡 工具链缓存清理（2026-10-29）：`raccoonci run -toolcache-limit-gb N`（默认 10，0 为不限），本机运行开始时按“最久没用”淘汰版本，直到不超上限；24 小时内用过的不删，所以上限是软的。“最近使用”记在 `.complete` 标记的修改时间上，每次选中某个版本都刷新；超过 24 小时的中断安装残留（`.part-*` 等）顺手清掉；采用的机器自带版本只是一串链接，不计大小。工具缓存在用户缓存目录（InstaBox 上是 `/data/home/.cache`，持久盘），不在 `/tmp`。**没做**：远程模式下机器上的缓存不清理（每台机器用完即删）；`node/`（Node.js 运行时，实测 750 MB）和 `runs/`（工作目录，765 MB，只增不减）不在这个上限里；只在运行开始时清一次，一次运行里装的东西不会在运行中被淘汰；setup-* 动作自己写进工具缓存的版本也会被淘汰，下次由它们重新下载

### 3.2 运行时服务（GHA action 依赖的内部接口）

- ✅ artifact v4（Twirp + Azure Blob 的 Put Block / Put Block List）：`internal/artifact`、`internal/blob`（本地目录和 S3 兼容存储）。`raccoonci run` 里 `upload-artifact@v4` 与 `download-artifact@v4` 跨 job 实测通过（20 MB，sha256 一致）
- ⬜ artifact 剩余：v3 REST；按 job 签发 token（现在整个 run 共用一个）；元数据持久化（现在在内存）；retention 到期清理（`expires_at` 只存不执行）；整体 sha256 校验（现在只记录）；服务端接 S3 并在真实 AWS 上验证（目前只测过 SeaweedFS）；`download-artifact` 的按 ID 跨 run 下载（需要 GitHub token，未做）
- ✅ cache v1（`/_apis/artifactcache`）：`internal/cache`。真实 `actions/cache@v4` 与 `cache/restore@v4` 实测（20 MB、1 MiB 分块、两个 job；第二个进程命中第一个进程存的缓存）。缓存按仓库放在用户缓存目录，跨次运行保留
- ⬜ cache 剩余：淘汰与总大小上限（现在只增不减）；按分支隔离（现在同仓库所有 ref 共用）；多个 `raccoonci` 进程同时写同一个 index（后写覆盖）；下载链接与 run 共用同一把密钥
- ✅ cache v2（Twirp `CacheService`：CreateCacheEntry / FinalizeCacheEntryUpload / GetCacheEntryDownloadURL）：`internal/cache/v2.go`，上传那一半抽成了 `internal/azblob`（新增单次 Put Blob）。`raccoonci run -cache-protocol v2` 下用真实 `actions/cache@v4` 实测：20 MB（单次 Put Blob，第二个进程命中）、150 MB（超过 SDK 单次上限 128 MiB，走分块），各跑两个 job。v1 与 v2 共用同一批缓存条目
- ⬜ cache v2 剩余：默认协议仍是 v1（toolkit 只有设了 `ACTIONS_CACHE_SERVICE_V2` 才走 v2，服务端不会按 toolkit 版本自动选）；请求里的 `CacheMetadata`（repository_id、scope）被忽略；150 MB 那次没有确认分块走的是直通路径还是落盘路径
- ⬜ OIDC token 端点（`ACTIONS_ID_TOKEN_REQUEST_URL`）
- ⬜ 真实的 `GITHUB_TOKEN`（GitHub App installation token，调用真实 GitHub API）
- ⬜ job summary 的存储与展示（现在只收集在内存里）、annotations 持久化

### 3.3 job 类型缺口（现在会给出明确报错，不会静默）

- ✅ 容器 job（`container:`）和 `services:`：`internal/docker`、`internal/runner/containers.go`。用真实 Docker 实测（`RACCOONCI_TEST_DOCKER=1`）：busybox 和 debian:bookworm-slim 作 job 容器、redis 作 service（健康检查、容器内按名字访问、宿主机 job 通过发布的端口访问）、容器内的本地 Node action、job 失败/镜像不存在/service 不健康时的清理，结束后没有残留容器和网络
- ✅ 容器 job 里的 artifact 和 cache 服务：job 网络建好后，服务在这张网络的网关地址（宿主机在这张网络上的地址）上再开一个监听，只接受这张网络子网内的连接；签发的上传/下载链接按请求进来的那个地址生成（`blob.WithBaseURL`），鉴权不变。实测：容器里 `upload-artifact@v4`（12 MB）+ `actions/cache@v4`（v1 和 v2 都测了），之后容器 job 和宿主机 job 各自下载并校验通过；其他 docker 网络和宿主机 eth0 地址连不上，同一网络里没有 token 返回 401；跑完后监听、网络、容器都没有残留。rootless docker 下不支持（网络不在宿主机上），会给出警告
- ✅ 取消容器里的步骤：步骤经 `sh -c 'echo $$ >pidfile; exec …'` 启动（docker 把每个 exec 放进自己的进程组，pid 即 pgid），取消时用另一次 `docker exec` 向容器内的进程组发 SIGINT，宽限期后发 SIGKILL。实测：步骤里的 `trap INT` 会执行，后台子进程随组退出；去掉这处修复后该测试会失败
- ✅ `docker login` 对真实私有仓库：本地 `registry:2` + htpasswd 认证，job 用 `credentials:` 拉取成功；密码错误则 job 失败且日志里没有密码；宿主机自己的 docker 配置没有被登录状态污染
- ✅ job 结束后 chown：断言容器里 root 写的文件归还给当前用户；把 chown 关掉后断言会失败
- ✅ Alpine（musl）镜像的 JS action：job 容器是 musl 时用 `unofficial-builds.nodejs.org` 的 musl 版 Node（同样校验 SHA-256，装在 `node/musl/` 下，随 `node/` 一起挂进容器）。musl 版动态链接 libstdc++，Alpine 没有，所以 Node 起不来时在容器里以 root 执行 `apk add libstdc++`（只影响这个 job 的容器，步骤日志有 notice）。实测真实 `upload-artifact@v4`/`download-artifact@v4` 在 Alpine 容器里跑通。`actions/cache` 在裸 Alpine 里会因 busybox 的 tar 失败，与 GitHub 上相同，镜像里需要 `apk add tar`
- ⬜ 容器 job 剩余：宿主机防火墙若拦截 INPUT（如 ufw 默认 deny），容器连网关会失败，目前没有检测；非 root 用户的镜像（`--user`）；被强杀的进程留下的孤儿容器没有清扫（容器带标签 `raccoonci=1`）；Alpine 的 `apk add` 需要容器能访问 Alpine 镜像源，离线或非 root 的 Alpine 镜像要自己预装 libstdc++；没有 `/bin/sh` 的镜像（distroless）上取消仍然只会杀 docker 客户端
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

- 🟡 机器上的 agent 和主控协议（`internal/dispatch`、`raccoonci agent`）：机器主动向主控发起 HTTPS 连接，领取任务、拉源码包、回传日志和心跳、报告退出码；不走 provider 的 exec（InstaBox 全账号 32 并发的限额），机器不需要公网地址。每个任务一个随机令牌（主控只存它的哈希，令牌只开自己的那个任务），日志按偏移量上传所以重试不重复、有缺口会拒绝；机器第一次联系超时（2 分钟）或之后 30 秒无声都会让 `Run.Wait` 报错（机器死了能发现）；agent 只接受 https（或回环地址的 http），因为令牌和机密都走这条连接；配置在 `/etc/raccoonci-agent.json`（令牌不放命令行）。用真实二进制测过：源码包过去、`actions/checkout` + `run` 步骤成功、失败退出码回传、机密在日志里被遮蔽。**还没做**：主控的对外进程和域名（开发期可用 `instabox-port` 发布，地址公开所以每个请求都鉴权，且只在这台机器运行时有效）；产物（artifact）和缓存在机器上跑完就丢，没有回传；机密现在随任务明文发到机器（要换成 Org Secret 占位符，或至少限定为机器专属令牌）；一次只一个任务，机器用完即删
- ⬜ 用户自己的机器（agent 连接）：协议已有，缺注册和长驻模式
- 🟡 InstaBox（Debian 13）：代码已写（`internal/instabox`：API 客户端 + `Provider`；`raccoonci run -on instabox -listen ADDR -controller-url https://… -snapshot ID`，密钥读 `INSTABOX_API_KEY`），一台机器跑整个 workflow。流程：从快照建机器（带 `deleteAfterSeconds` 兜底，主控进程死了服务端也会删）→ 等操作完成 → 文件接口写 `/data/home/.raccoonci-agent.json`（令牌）→ 快照里的 systemd 服务 `raccoonci-agent` 读到后连主控 → 跑完删机器（取消也会删）。API 规格来自 `https://api.instabox.sh/v1/openapi.json`（公开，能查全部接口）。新快照 `raccoonci-runner-agent-23db1cb`（`sn_EXAMPLE_SNAPSHOT_ID`）带 agent 服务；机器内 home 是 `/data/home`，不是 `/home/user`。**真实端到端已跑通（2026-10-29）**：本项目自己的 `ci.yml`（checkout、setup-go、gofmt、`make check`、build、upload-artifact）用 `-on instabox` 在新建机器上成功，job 用时 1m15s，整个命令 1m35s（含建机器、传源码、删机器），机器事后已不在列表里；主控通过 `instabox-port` 发布的地址被机器连上。API 要求所有写操作带 `Idempotency-Key`（客户端每次随机生成）。密钥读 `INSTABOX_API_KEY`，没有则读 `IBX_API_KEY`（`INSTABOX_API_KEY` 是机器上的保留名，org Secret 只能用别的名字；shell 里是占位符，代理换头）。**现在是一个 job 实例一台机器（2026-10-29）**：引擎在主控这边解析 needs 和矩阵（`engine.Options.Remote` 收到每个已解析的实例），每个实例发给一台新机器，机器上 `raccoonci run-instance` 用 `engine.RunInstance` 跑（装环境、选镜像、runner 都在机器上），事件按 JSON 行流回来、结果带 outputs 回来（`wireReader`，`value.FromJSON` 保持键顺序），所以 `needs.*.outputs`、动态矩阵、fail-fast、`max-parallel` 都照常。artifact 和 cache 服务由主控用公开地址提供（`publicServe`，链接和环境变量都用 `-controller-url`），机器直接访问，所以 job 之间的产物/缓存通了。实测 cobra 全部 workflow：10 台机器并行（lic-headers、golangci-lint、8 个 ubuntu 矩阵），整条命令 56 s，ubuntu 实例各 9–18 s，8 个 macOS 和 1 个 Windows 实例按设计在主控这边直接失败，没有占机器；10 台并发创建时机器就绪 6–22 s（单台约 6 s），事后全部删除。runner 快照现在是 `raccoonci-runner-ri-4008bd4`（`sn_EXAMPLE_SNAPSHOT_ID`），旧的两个已用 API 删掉。`-parallel` 在远程模式默认 8。**收尾（2026-10-29）**：源码包不再带 git 忽略的文件（`.git` 保留；不是 git 仓库时不排除）：测试里放了 30 MB 的被忽略文件，包只有 12 KB。远程模式下每个实例有自己的服务令牌（job id 不同，只能以自己的 job 创建 artifact，不再是整个 run 共用一个；同一 run 的别的 artifact 仍然读得到，GitHub 也是这样）。runner 快照可以自动重做：`make runner-binary VERSION=x` 编静态二进制，`raccoonci runner-snapshot [-binary …] [-replace 旧id]` 起一台干净的 `universal`、装二进制和 agent 服务、检查版本和静态链接、拍快照、删机器（实测 11 s）。**限制**：机器上的日志和主控离得远时没有压缩；`run` 时还不校验快照清单里的 `raccoonci` 版本和模板版本。API 有 `DELETE /v1/snapshots/{id}`（key 需要 `images:delete`），所以旧快照可以由我们自己删，不必手工
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

- ✅ 用 Raccoon CI 自己跑自己的 `make check`（dogfood）：`.github/workflows/ci.yml`（checkout、`setup-go` 用 `go-version-file`、gofmt、`make check`、`make build`、`upload-artifact`），在本机 1 分钟左右跑通。这次暴露并修复了一个真问题：缓存/artifact 服务暂存分块默认放在 `/tmp`，本机的 `/tmp` 是 2 GB 的 tmpfs，保存一份 Go 构建缓存（约 750 MB 压缩）时写满，服务端回 500，`actions/cache` 只报 “responded with 500” 和一串 EBADF。现在暂存目录放在缓存/artifact 仓库旁边的持久盘上（`azblob.NewSpoolDir`，每个进程一个私有子目录，超过一天的残留自动清理）
- 🟡 兼容性语料：对 13 个真实开源项目的 90 个 workflow 文件跑了 `plan`，全部解析并展开通过。真正跑起来的有 `spf13/cobra` 的 `test-unix`（8 个 Go 版本 × ubuntu 全部通过，8 个 macOS 实例按设计明确失败）。**还没跑**：commander.js（Node，actions 用 commit SHA 固定）、click（Python + uv + tox）、express、需要 Rust 的项目（没有 Rust 安装器，语料里约 60 个实例需要它）
- ⬜ 已知：同一台机器第二次跑时，`setup-go` 还原缓存会因为 `~/go/pkg/mod` 里已有的只读文件而报 “File exists”（tar 退出码 2，只是警告，job 照常成功）。这是长期使用的自托管机器上 `setup-go` 的已知行为，GitHub 的全新虚拟机不会遇到；对“用户自己的机器”这条路线会常见，需要想办法（例如每次 job 用干净的 HOME，或者还原前清理）
- ✅ 服务端 5xx 的原因以前不会出现在任何日志里：现在 `raccoonci run` 会把 artifact/cache 服务的 5xx 连同原因打到 stderr（`local.LogServerErrors`，只记路径，不记签名）。靠它确认了 cobra 矩阵里 `1.22.x` 的缓存提交失败是 `no space left on device`（`/data` 最低剩 366 MiB）
- ✅ 缓存大小上限和淘汰：`-cache-limit-gb`（默认 10，0 为不限），超过时淘汰最久没被命中的条目；比上限大的条目直接拒绝；提交前检查磁盘剩余空间，不够就先淘汰，淘汰完还不够返回 507。v1、v2 两条协议都覆盖，单测包含淘汰顺序、索引持久化、磁盘不足
- ⬜ 缓存提交时仍要在仓库目录里再写一份完整副本（暂存 + 仓库，瞬时 2 倍）：`Dir` 仓库可以直接把暂存文件改名进仓库，省掉那份拷贝；淘汰只在单进程内有效，多个 `raccoonci` 进程共用同一个缓存目录时各自的上限互相看不见
- ✅ 引擎：不可用 OS 的实例不再占并发槽位排队，立即失败（实测 cobra 的 8 个 macOS 实例一开始就全部报错）
- 🟡 硬编码原则：evolving 的名单都已进 catalog；GitHub 协议本身（字段名、workflow 命令名、上限）和下载地址有意留在代码里，原因见 `CLAUDE.md`
