# Raccoon CI cli 仓库：进度与待办

更新于 2026-11-03。`make check` 全绿，CI 通过（依赖已发布的 lib 和 runner `v0.1.0`）。

图例：✅ 完成　🟡 部分完成　⬜ 未开始

本文件是从单体仓库的 TODO 里拆出来的，只留属于 cli 仓库（`raccoonci` 命令、`internal/{envsnap,plan}`）的条目：

- **A. 作为 GitHub Runner**（`raccoonci serve`）：实现已搬到 lib 仓库的 `pkg/ghrunner`，这里只有命令入口。
- **B′. `raccoonci run -on instabox` 的 cli 一侧**（`cmd/raccoonci/{remote,runremote}.go`、`internal/envsnap`；`pkg/sched` 在 lib）。
- 自有引擎本身（原 B）在 runner 仓库；服务端（原 C.1）在 backend 仓库；算力提供方 `pkg/instabox`、catalog 与 catalog feed（原 C.2）在 lib 仓库。下文提到"B.x / C.x / D.x"处指原单体 TODO 的编号，对应条目已搬到那几个仓库。

两条路（A，和自有引擎）哪条是主线还没定，**两条都不删，也不把任何一条叫"那个"架构**。

---

# A. 作为 GitHub Runner（`raccoonci serve`）

实现（`pkg/ghrunner`、`pkg/sched`）已经搬到 lib 仓库，A.1–A.4 的完成情况、基准、未验证项和加速边界也一起搬去了 lib 的 `TODO.md`。本仓库只留 `serve` 命令的入口（`cmd/raccoonci/serve.go`、`webhooks.go`）。

# B′. `run -on instabox`（cli 一侧）

- ✅ **`-on instabox` 已在真机验证（2026-11-03）**：当前拆分后的二进制，用 `raccoonci run -on instabox` 在 InstaBox 真机上跑通，花费约 $0.0016。（2026-10-29 的首次端到端：本项目 `ci.yml` 在新建机器上成功，job 1m15s，整个命令 1m35s。）
- 🟡 InstaBox（Debian 13）：代码已写（`pkg/instabox`：API 客户端 + `Provider`；`raccoonci run -on instabox -listen ADDR -controller-url https://… -snapshot ID`，密钥读 `INSTABOX_API_KEY`），一台机器跑整个 workflow。流程：从快照建机器（带 `deleteAfterSeconds` 兜底，主控进程死了服务端也会删）→ 等操作完成 → 文件接口写 `/data/home/.raccoonci-agent.json`（令牌）→ 快照里的 systemd 服务 `raccoonci-agent` 读到后连主控 → 跑完删机器（取消也会删）。API 规格来自 `https://api.instabox.sh/v1/openapi.json`（公开，能查全部接口）。新快照 `raccoonci-runner-agent-23db1cb`（`sn_EXAMPLE_SNAPSHOT_ID`）带 agent 服务；机器内 home 是 `/data/home`，不是 `/home/user`。**真实端到端已跑通（2026-10-29）**：本项目自己的 `ci.yml`（checkout、setup-go、gofmt、`make check`、build、upload-artifact）用 `-on instabox` 在新建机器上成功，job 用时 1m15s，整个命令 1m35s（含建机器、传源码、删机器），机器事后已不在列表里；主控通过 `instabox-port` 发布的地址被机器连上。API 要求所有写操作带 `Idempotency-Key`（客户端每次随机生成）。密钥读 `INSTABOX_API_KEY`，没有则读 `IBX_API_KEY`（`INSTABOX_API_KEY` 是机器上的保留名，org Secret 只能用别的名字；shell 里是占位符，代理换头）。**现在是一个 job 实例一台机器（2026-10-29）**：引擎在主控这边解析 needs 和矩阵（`engine.Options.Remote` 收到每个已解析的实例），每个实例发给一台新机器，机器上 `raccoonci run-instance` 用 `engine.RunInstance` 跑（装环境、选镜像、runner 都在机器上），事件按 JSON 行流回来、结果带 outputs 回来（`wireReader`，`value.FromJSON` 保持键顺序），所以 `needs.*.outputs`、动态矩阵、fail-fast、`max-parallel` 都照常。artifact 和 cache 服务由主控用公开地址提供（`publicServe`，链接和环境变量都用 `-controller-url`），机器直接访问，所以 job 之间的产物/缓存通了。实测 cobra 全部 workflow：10 台机器并行（lic-headers、golangci-lint、8 个 ubuntu 矩阵），整条命令 56 s，ubuntu 实例各 9–18 s，8 个 macOS 和 1 个 Windows 实例按设计在主控这边直接失败，没有占机器；10 台并发创建时机器就绪 6–22 s（单台约 6 s），事后全部删除。runner 快照现在是 `raccoonci-runner-ri-4008bd4`（`sn_EXAMPLE_SNAPSHOT_ID`），旧的两个已用 API 删掉。`-parallel` 在远程模式默认 8。**收尾（2026-10-29）**：源码包不再带 git 忽略的文件（`.git` 保留；不是 git 仓库时不排除）：测试里放了 30 MB 的被忽略文件，包只有 12 KB。远程模式下每个实例有自己的服务令牌（job id 不同，只能以自己的 job 创建 artifact，不再是整个 run 共用一个；同一 run 的别的 artifact 仍然读得到，GitHub 也是这样）。runner 快照可以自动重做：`make runner-binary VERSION=x` 编静态二进制，`raccoonci runner-snapshot [-binary …] [-replace 旧id]` 起一台干净的 `universal`、装二进制和 agent 服务、检查版本和静态链接、拍快照、删机器（实测 11 s）。**限制**：机器上的日志和主控离得远时没有压缩；`run` 时还不校验快照清单里的 `raccoonci` 版本和模板版本。API 有 `DELETE /v1/snapshots/{id}`（key 需要 `images:delete`），所以旧快照可以由我们自己删，不必手工
- 🟡 项目环境快照（装完依赖拍快照，锁文件不变就复用）：键和登记表已做（`internal/envsnap`）。键 = 锁文件内容哈希 + 用到的工具链名 + 机器镜像标识，不含工具链版本；`plan` 会显示每个实例能否快照。真实项目上：cobra/本项目（go.sum）、commander.js（package-lock.json）、click（uv.lock）可以；express 没有锁文件，判定为不能快照。**已接到 `-on instabox`（`-env-snapshots N`，默认关）**：实例启动前按键查登记表，命中就从该快照建机器，快照已被删（400/404）则忘掉并退回 runner 快照重跑；未命中时，成功的一次运行会在后台清理机器（删 agent 配置和本次 workspace，保留各种缓存）、拍快照、再删机器，同一个键同时只有一台机器拍；超出上限的旧快照由 API 删除。只在可信事件（非 `pull_request*`）、且没有 `-secret` 时才拍。**monorepo**：键只含仓库根的锁文件，所以用到的工具链在子目录里有清单或锁文件（跳过 `.git`、`node_modules`、`vendor` 等）时，判定为不能快照（`ErrUnpinned`），避免子目录依赖变了却复用旧快照；把子目录锁文件也算进键还没做。**还没做**：**真实 InstaBox 端到端已验证（2026-10-29）**：带 uuid 依赖的小 Go 项目，三次运行：①未命中，建机器 10.8 s，构建 4.2 s（下载依赖），成功后保存快照；②命中，机器 7.7 s，构建 0.3 s，整条命令 35 s → 10.6 s；③改 go.sum 后键变了，未命中并保存新快照，旧快照被淘汰（上限 1）。测试机器自动删除，测试快照和端口已手工清理。**另一个实测发现（本项目 ci.yml，`-on instabox`）**：`actions/setup-go` 的缓存（747 MB，含 go-build）从主控恢复用了 2m26s，比什么都不缓存的冷跑（总共 1m45s）还慢——主控（这台 InstaBox 机器）出口约 6 MB/s。所以在"主控本身在 InstaBox 上"的开发配置里，开着环境快照时 `setup-go` 的缓存恢复反而拖慢整个 job；正式的主控要放在带宽足够的地方，或把缓存对象放对象存储，机器直接从存储下载。**缓存对象放 S3 已做（2026-10-29）**：`run -cache-s3-bucket B [-cache-s3-endpoint URL -cache-s3-path-style -cache-s3-region R -cache-s3-prefix P]`，密钥只从环境变量 `RACCOONCI_CACHE_S3_KEY`/`_SECRET`（可选 `_TOKEN`）读；每个仓库一个子目录；下载是预签名链接，机器直接找存储；上传仍经过主控（缓存协议是对 Azure Blob 的模拟，直传要另改）。用本地 SeaweedFS 的 S3 网关验证了功能：1 GB 的 setup-go 缓存保存成功、第二次运行能找到并下载。**没有在真实外部桶上量过速度**——本地 SeaweedFS 和主控在同一台机器上，量不出带宽差别。在 InstaBox 机器上实测：入站从公共 CDN 下载 78 MB 用 0.42 s（约 185 MB/s），出站约 6 MB/s 且是每台机器各自的限制（两台同时传互不影响，同机两个并发上传共享这 6 MB/s），所以缓存下载应该会快很多，保存缓存（上传）不会。验证中发现并修了一个真问题：S3 的分片是流（`LimitReader`），SDK 重试时倒不了带（"request stream is not seekable"），存储偶尔 5xx 就让保存直接失败；现在不超过 32 MiB 的分片先读进内存再发（`maxBufferedPart`）
- ✅ 短 job 合并到同一台机器（2026-10-29）：`pkg/sched`（现在在 lib 仓库）把每个实例在机器上的耗时（从开始到最后一步，不含机器启动）按"workflow + 实例名"记下最近 5 次，取中位数；估计值低于机器启动时间（本次运行已测到的中位数，没有就按 10 s）的 3 倍就算短。短实例交给 `instabox.Pool`：先填满已有（或正在启动）的机器再开新的，每台机器同时跑的数量默认等于 vCPU 数（`-pack-slots N`，1 表示一个接一个），空闲机器留 15 s 给紧接着就绪的下游 job，run 结束时全部删除。`-no-pack` 关闭。没有历史的实例算长（独占一台机器），所以第一次运行和以前一样，第二次起才合并。用到 `container:`/`services:` 的 job 和能用环境快照的实例不合并。runner 快照里的 agent 现在是 `raccoonci agent -loop -slots 8`：每个槽位一个配置文件（`dispatch.SlotConfig`），读完就删，跑完等下一份。**需要重建 runner 快照**（旧快照的 agent 只跑一个任务）。真机结果（`monorepo` 夹具，同一台沙箱发布控制器，新 runner 快照 `sn_EXAMPLE_SNAPSHOT_ID`，各跑 1 次）：第 1 次（无历史）3 台机器、48 s、$0.0008；第 2 次 2 台、37 s、$0.0008（backend 上次 32.7 s 没低于 30 s 的线，独占）；第 3 次 1 台、34 s、$0.0004。每个 job 一台机器时是冷 55 s / $0.0009。**还没做的**：没有用 `raccoonbench` 跑完整的冷/warm/change 三场景、也没重复取中位数，所以和"更早版本一台机器"的 17 s warm 比还没有结论；每台机器上的 Go 模块缓存是冷的，这是和那台长期机器的主要差别；并行槽位里的 job 若绑定固定端口会互相冲突（目前只排除了 container/services）；预热机器池没做，等有真实使用数据。

## 其它历史事实

- 基准在 https://github.com/Raccoon-CI/raccoonbench（它从 checkout 构建 `raccoonci`，读 `run -usage-file` 的输出和 `results ready after N s` 等行，改这些格式要同步改它）。
- 许可证 Apache-2.0（用户 2026-11-02 定）。拆仓库：名字、可见性和推送由用户决定，现在什么都没推。
