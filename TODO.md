# Raccoon CI 进度与待办

更新于 2026-11-01。`make check` 全绿。

图例：✅ 完成　🟡 部分完成　⬜ 未开始

## 总览：三个部分

按"这件事是为哪条路做的"分成三块，每块各管各的待办：

- **A. 作为 GitHub Runner**（`raccoonci serve`，`internal/ghrunner`）：GitHub 触发并编排，我们只提供 runner 和机器。
- **B. 作为独立 CI/CD 平台**（`raccoonci run`，自有引擎）：Raccoon CI 自己解析并执行 workflow，所以要自己实现 action 依赖的运行时服务、job 类型和 workflow 语义。
- **C. 服务端与算力对接**：长期运行的服务（事件入口、调度、数据库、UI、机密）和算力提供方（InstaBox、用户自己的机器、其他云）。B 要变成"平台"需要它；A 用到它的算力部分（起机、限额、规格）。

**当前目标（2026-11-01 用户定）：先写服务端（C.1），写完再拆成独立仓库**——raccoonci-frontend、backend、raccoonci-runner、cli，共用的代码单开一个库仓库；独立指独立仓库和发版，代码会开源、多人维护。设计草案见 `docs/server-design.md`。

两条路（A、B）哪条是主线还没定，**两条都不删，也不把任何一条叫"那个"架构**。放在哪块的判断：只有 A 用得上的放 A；只有自有引擎用得上的放 B；两边都要的算力和服务端放 C；两边都能用的专项优化（如 Node 家族的缓存）放在 A.4，并注明 B 也能用。

| 部分 | 阶段 | 状态 | 说明 |
|------|------|------|------|
| A | GitHub 触发路径（`raccoonci serve`） | 🟡 | 2026-10-31 起。GitHub 触发并编排，`raccoon-*` 标签的 job 由 InstaBox `ci` 模板机器上的 GitHub runner 执行；保活、提前启动、组织级共享池、webhook 都已实测；缺更大规模验证（见 A）。**和"自己的引擎"哪条是主线还没定** |
| B | workflow 解析 + matrix 展开 | ✅ | 含表达式求值 |
| B | 依赖扫描器 | ✅ | |
| B | 单机执行器 | 🟡 | `run:`、JS/composite/Docker action、容器 job 和 `services:` 可用；可复用 workflow 不支持 |
| B | 装环境（按扫描结果安装） | 🟡 | Node.js、Go、Python、Java、Rust、apt 包已通；Ruby 和各种 CLI 工具还没有安装器（已搁置，见 B.2 末尾） |
| B | artifact v4 + cache v1/v2 服务 | ✅ | 两者在本机运行里都用真实 action 实测通过；v3 REST、持久化/淘汰等见 B.3 |
| C | InstaBox provider | 🟡 | 自己的引擎跑在 InstaBox 机器上已通（见 C.2）；缺服务端（C.1） |
| C | 服务端 | ⬜ | 完全没开始（C.1） |

---

# A. 作为 GitHub Runner（`raccoonci serve`，`internal/ghrunner`）

背景：2026-10-31 用户研究 Blacksmith 后认为"只用自己的引擎、不做 GitHub runner 编排"的判断可能有误，要求研究并做出这条路径。代码在 `internal/ghrunner`，命令 `raccoonci serve`，设计和数字见 `docs/research/idle-runners/README.md`，架构说明见 `CLAUDE.md`。InstaBox 的 `ci` 模板（Ubuntu 24.04 = GitHub 官方 runner 镜像，runner 2.338.2 未注册）已上线，所以操作系统和镜像不再是问题。

## A.1 已完成与实测

- ✅ 主流程：按仓库轮询排队的 job（ETag 条件请求，304 不占额度）→ 标签前缀 `raccoon-` 的 job 生成 JIT runner + 起机器（并行）→ JIT 配置写进机器文件（0600，不进命令行和日志，测试断言）→ 跟到 GitHub 把 runner 删掉 → 删机器。真实跑通：job 成功，机密没有出现在日志里
- ✅ 实测启动时间（GitHub 报告的 queued→started）：现建机器 10 s；暂停的备用机（`-warm N`）7–9 s；已在线的空闲 runner 1–2 s。对照：GitHub 托管 3–4 s，InstaBox 官方 `instabox-medium` 12–20 s
- ✅ 冷启动时间花在哪（2026-10-31，读机器上 runner 自己的日志）：排队到开始 6–9 s，GitHub 托管 2 s。机器创建 1.6 s + runner 进程启动 1–2 s + runner 向 GitHub 认证并建立会话约 2 s + GitHub 把 job 派给它约 1 s + 我们发现不到 0.5 s。暂停的备用机已经没有价值：恢复要 1.6–3.2 s，不比新建快，runner 自己的启动在热机器上也没快；`-warm` 默认就是关的，保持关闭。剩下的大头是 GitHub 的 runner 协议，只能用等待中的 runner 遮住（1–2 s），去不掉
- ✅ 自适应保活（`keepalive.go`）：模型和在真实到达轨迹上的比较见研究笔记。每隔一段到达的间隔，按最近 100 次间隔选使"等待成本 + 延迟价值"最小的 TTL，不到 20 次用 ski-rental 保本点 `V·Δ/C`；池大小跟随到达的突发规模（最多 4）；多余的等待 runner 先删注册再退役（有 job 的 runner GitHub 不让删，所以安全）。`-second-value`（默认 0.01 美元/秒，0 关闭）、`-idle N`（下限）
- ✅ 已知到达提前启动（`prestart.go`、`cron.go`）：`schedule:` 的 cron 到点前 8 秒起 runner（保留到之后 10 分钟，GitHub 的定时 run 常晚几分钟）；`needs` 某个正在跑的 job 的下游 job，按上游最近几次时长的中位数在它结束前起 runner（没有历史就不猜）。真机：链式 a→b→c 第二次运行，b、c 排队到开始 2 s（第一次冷启动 a 16 s、因保活 b、c 也是 2 s）
- ✅ 被别的等待 runner 抢走 job 的 runner 约 10 秒后释放（以前要白等 3 分钟的 `IdleTimeout`）。实测中发现并修复；真机上这个修复本身还没重跑过，只有单测
- ✅ 组织级共享池（`-org`）：runner 登记在组织的 `Default` 组（私有仓库可用），一个仓库的突发留下的等待 runner 其他仓库也能用。真机：`bench-lint-docs` 的 job 之后，`bench-go-integration` 的 job 排队后 2 s 开始，用的是保留下来的 runner。只写 `-org` 不写 `-repo` 时监控组织里没归档的全部仓库
- ✅ 按标签选机器规格（2026-10-31，`size.go`）：`raccoon-<N>vcpu` 起 N 核的机器，内存取 InstaBox 给的那一档（可用规格从 `GET /v1/orgs` 读，这个账号是 2/4、4/8、8/16 GiB）；没写规格的用默认。账号建不出的规格（`raccoon-64vcpu`）不会让 job 一直排队，而是在日志里写明能建什么并取消该 run。等待中的 runner 只有 `self-hosted` 和 `raccoon-4vcpu` 两个标签，所以只接这个标签的 job；其他规格的 job 一律新建机器（冷启动），保活和共享池暂时只管默认规格。真机：`raccoon-8vcpu` 的 job 里 `nproc`=8、内存 16 GB；`raccoon-64vcpu` 被拒并取消，没建机器
- 🟡 **webhook 取代轮询**（2026-10-31，`webhook.go`）：`serve -webhook-listen ADDR -webhook-url https://…`，给每个监控的仓库建/更新一个 `workflow_job` hook（`admin:repo_hook`，退出时删除），HMAC-SHA256 验签（错误签名一律 401，不读内容），队列中的 job 直接进和扫描同一个判断函数（重复投递、扫描同时发现都不会起两台），其余事件唤醒扫描；有 webhook 时每 30 秒（`-safety-poll`）才列一次仓库。顺带修了：启动失败的 job 以前不会重试。单测覆盖（签名、不轮询也能起机器、重投、非监控仓库、hook 注册与删除）。**真机已验证（2026-10-31）**：用户用 `instabox-port` 发布 18091 端口（我自己发布被权限拦下，没有绕开），GitHub 的 ping 回 204，未签名的 POST 经公网地址回 401，`bench-lint-docs` 的 a→b→c 推了两次：job 的 runner 在投递到达后 1.4 s 起好，冷启动的第一个 job 排队到开始 7 s（轮询时 10–16 s），后续 job 1–2 s；`serve` 结束后 hook、runner、机器都已清掉。没测：GitHub 投递失败时靠 30 秒兜底列表找回 job 的路径（只有单测）。组织级 hook 需要 `admin:org_hook`，现在是每个仓库一个。端口 18091 还发布着，由用户决定何时撤下
- ✅ 在 `push`/`pull_request` 事件到达时提前起 runner（2026-11-01，`event.go`，已提交）：webhook 现在订阅 `workflow_job`、`push`、`pull_request`；事件一到就读该提交上的各个 workflow，用 `trigger.Match`（分支、tag、`paths`、活动类型过滤，变更文件走 compare 接口，取不到就当未知）判断哪些会被触发，数出它们第一层、带我们标签、不受其他规格限制的 job（矩阵按个数算），立刻提前起那么多 runner，保留 1 分钟。基准用的 `workflow_dispatch` 没有这个事件，所以基准看不出来。**真机实测（`bench-lint-docs`，6 次 push）**：GitHub 从事件到把第一个 job 排上队稳定是 **2–3 s**（以前凭 1–3 s 猜）；提前起了 runner 的第一个 job 排队→开始是 5–7 s（冷启动是 6–9 s），**实际只快了约 1–2 s**，样本只有 6 次、在噪声范围内；6 台里有 1 台没被用上。原因是 runner 从起机到能接任务要约 7 s，而 GitHub 只提前了 2–3 s 给我们。想要更多就只能让 runner 更早在线（等待中的 runner，要花钱）。`pull_request` 没有真机测：刚开的 PR 的 `refs/pull/N/merge` 读不到（404，GitHub 异步生成合并提交；`refs/pull/N/head` 可以），所以 PR 事件读 workflow 用的是 PR 头提交。没做：静态求值 job 的 `if:`（为假的 job 也会起 runner）

## A.2 基准（serve 与 GitHub 托管、InstaBox 官方 runner 的对比）

基准在 https://github.com/Raccoon-CI/raccoonbench。**基准的原则（用户 2026-10-31）：保持客观，不能为了我们的产品改形状；结果不好就改产品。**自有引擎的基准见 C.3。

- ✅ 完整对比（2026-10-31）：7 个 bench 仓库都有了 `bench-raccoon` 分支（`runs-on: raccoon-4vcpu`，和 `bench-github` 只差这一行），3 个有 `bench-raccoon-2c`（`raccoon-2vcpu`）；raccoonbench 新增 `raccoon-serve` 系统（`serve` 随会话启停，花费按 serve 的用量文件算）；价格文件改成 GitHub 当前价（2 核 $0.006/分钟，4 核 $0.012），`instabox-medium` 按 4 vCPU / 8 GiB 的模板估（推断）。第一轮：7 负载 × 4 系统 × 1 次；第二轮：3 负载 × 3 系统 × 3 次；第三轮：2 核对 2 核，3 负载 × 3 次。Blacksmith 和 Jog 因为要企业资质/预充 100 美元，已决定不比
- ✅ 第二轮基准（2026-10-31，3 个负载 × 3 个系统 × 每格 3 次取中位数，GitHub 托管是 2 核、其余 4 核）：和 InstaBox 官方 runner 比，serve（webhook）在 warm 和 change 场景稳定快 2–12 s（lint-docs warm 13 对 18 s，go-integration warm 26 对 32 s，typescript-monorepo warm 52 对 64 s），原因是排队只有 1–2 s 而官方是 6–11 s；cold 场景两者相当（18/20、49/47、75/74 s）。和 GitHub 托管比，重负载明显更快（typescript-monorepo warm 52 对 111 s，go-integration cold 49 对 72 s，主要来自 4 核对 2 核），短 job 的 cold 慢约 4 s（lint-docs 18 对 14 s）。花费上 serve 比官方 runner 的估算高 3–6 倍（它是 job 秒数 × medium 规格的估算，serve 是整台机器的寿命，含启动、等待 runner 和 50 GiB 根盘），但仍只有 GitHub 托管的 5%–20%；两边口径不同，不是严格可比
- ✅ 第三轮基准，2 核对 2 核（2026-10-31，GitHub 托管 `ubuntu-latest` 对 `raccoon-2vcpu`，3 个负载 × 每格 3 次取中位数，`serve -runner-label raccoon-2vcpu` 让等待的 runner 也是 2 核）：lint-docs cold 20 对 16 s（慢 4 s）、warm 14 对 15、change 14 对 15；go-integration cold 62 对 72、warm 27 对 42、change 34 对 46；typescript-monorepo cold 107 对 108、warm 67 对 103、change 75 对 106。花费约为 GitHub 的 2%–15%。**换成同样核数后，warm 和 change 仍然明显更快**，逐步骤对比（typescript-monorepo warm）：`pnpm install` 4 s 对 13 s，`pnpm build` 24–25 对 28–43 s，queue 都是 1–2 s。**原因（已用硬件测试证实，见下一条）：这个优势来自 InstaBox 的硬件，不是我们的调度**。我们自己的贡献在排队：有等待 runner 时 1–2 s，对 InstaBox 官方 runner 的 6–11 s（第二轮）。cold 的短 job 仍然慢 4 s，是 GitHub 协议的固定开销
- ✅ 硬件对比（2026-10-31，同一个脚本在两边各跑 3 次，GitHub `ubuntu-latest` 对 `raccoon-2vcpu`，都是 2 vCPU）：GitHub 托管是 AMD EPYC 7763、8 GB、根盘 `/dev/sda1` ext4；InstaBox 是 AMD EPYC、4 GB、`/dev/vda` ext4。3 万个 4 KiB 小文件：读 0.34 对 0.13–0.15 s（快约 2.4 倍），整树复制（缓存还原解包做的事）3.0–3.15 对 1.2–1.4 s（约 2.3 倍），全部硬链接（`pnpm install` 在 store 已热时做的事）0.75 对 0.17–0.19 s（约 4 倍），删除 0.78 对 0.30–0.35 s（约 2.4 倍），创建 1.4–1.5 对 0.69–1.99 s（波动大，一次比 GitHub 慢）；单线程 sha256 300 MB 0.20 对 0.14–0.17 s（约快 20%–30%）；顺序写 189–209 对 252–524 MB/s。结论：小文件元数据操作快 2–4 倍，单线程 CPU 快两成左右，这正好对上 warm 的 `pnpm install` 从 13 s 到 4 s。**限制**：每边只有 3 次；"清缓存后顺序读"在 InstaBox 上是 1.6–2.6 GB/s，太高，可能没有真的绕过宿主机缓存，不能当磁盘速度用；InstaBox 那边的 CPU 型号被截断，没确认是不是同一代。这个优势 InstaBox 官方 runner 也有，所以它不能算我们相对官方 runner 的差异
- ⬜ 剩下 4 个负载（docker-build、frontend-e2e、security-sbom、rust-large）的 2 核对比：用户判断重负载上已有答案（第一轮 4 核数据覆盖了全部 7 个负载），**不再跑**。报告里只有 3 个负载的 2 核数据，要如实写明，不能说成全部
- ✅ 清理（2026-11-01，用户同意）：组织里三个空的 `Blacksmith runners …` runner 组（0 个 runner、0 个仓库）已删，只剩 `Default`；`bench-frontend-e2e` 的分支 `blacksmith-migration-2b5b7fd` 已删，它上面 Blacksmith 机器人开着的 PR #1 先关闭。**还在组织里的**：Blacksmith、InstaBox、Jog 三个 GitHub App（没人要求动，未处理）；`bench-lint-docs` 里我为验证 `refs/pull/N/merge` 开了又关的 PR #1（GitHub 不能删 PR）

## A.3 未完成与未验证

- ⬜ **提高每分钟起机数量**（用户 2026-11-01 在查；和 C.2 的 InstaBox 限额是同一件事）：InstaBox Pro 每分钟最多起 10 台，是矩阵 fan-out 的瓶颈（见 A.4 的矩阵个数一段）。要确认能否提高（更高的套餐、或向 InstaBox 申请）。查到之前的备选：同一个 run 内复用机器（给刚跑完的机器换一份新的 JIT 配置，不占起机名额，代价是同一个 run 的 job 之间不再隔离，默认应关）；`-starts-per-minute` 已能跟着改
- ⬜ InstaBox 30 分钟自动挂起会不会把"有 runner 在监听的机器"当成空闲，没测
- ⬜ 真实 `schedule:` 触发没测（cron 计算和提前启动只有单测）
- ⬜ `actions/cache` 和 artifact 在这条路径上走 GitHub 自己的服务；能否重定向到 `internal/cache` 没试
- ⬜ `pull_request` 事件没在真 PR 上测；run 的 `path` 的实际内容也没在真 PR 上验证（见 A.4 末尾）
- ⬜ 组织级 webhook（`admin:org_hook`）；保活和共享池按机器规格分开（现在只管默认规格）
- ⬜ 静态求值 job 的 `if:`，为假的 job 不起机器；同一个 run 里让下游 job 接着在上一个 job 的机器上跑；边车（从机器上读上游 job 的真实输出）

## A.4 两种 trigger 下的加速边界，和 Node 家族的专项优化（2026-10-31 讨论，**都还没做**）

**走 Actions trigger 时做不了的加速**（GitHub 决定"跑什么、什么时候跑"，我们只提供机器）：①跳过没受影响的 job 和按内容哈希重放整个 job（runner 只执行 GitHub 发来的步骤，不能改、不能省；取消会让 run 变 cancelled）；②去掉 GitHub 自己的派发延迟（冷启动约 4–5 s：建 run 排队 1–3 s、runner 认证建会话约 2 s、派发约 1 s；有等待 runner 时下限 1–2 s）；③提前精确知道整个任务图（动态矩阵、依赖上游输出的 `if:` 要等运行时；静态部分可以读 workflow 文件得到，下游 job 的时间只能预测）；④控制排队顺序和优先级（`concurrency`、队列是 GitHub 的）；⑤同一个 run 的 job 之间原生共享工作区和服务容器；⑥替换 GitHub 的缓存、artifact、OIDC、job summary 服务（令牌和地址来自 GitHub 的 job 消息，可能能用 runner 的 `.env` 或 job-started 钩子重定向，没试过）；⑦替换 `actions/checkout`。**能做但没做**：同一台机器上多个一次性 runner 打包短 job、环境快照（sticky disk 式）、同一个 run 内复用机器、按规格保活、push/PR 事件到达时提前启动。**只有自己的 trigger 才能做**：跳过没受影响的 job、没有 GitHub 的派发下限、精确的任务图、排队控制。**自己的 trigger 做不了或代价更高**：GitHub 的全部语义（可复用 workflow、`concurrency`、环境和保护规则、OIDC、必需检查、PR 界面）、macOS/Windows/ARM（GitHub runner 装在用户自己的机器上就行）。

**"跑了但实际没跑"（Actions 下跳过没受影响的 job）的两种做法**：①改造普通 runner：把 `bash`/`sh`/`python` 和 runner 自带的 `node` 换成直接退出 0 的小程序，界面上每步显示成功，要靠日志和注解说明；②自己实现 runner 协议的假 runner，把每步报成 Skipped、job 报成功，可以放在控制器上不起虚拟机（约 3 s），但协议没有官方文档（`github-act-runner` 是开源实现，没验证）。判断"该不该跳"用现有的 `internal/affected`（见 B.6），输入是变更文件列表和仓库的包依赖图。**最大风险是假绿**，所以要：默认关闭、按仓库开启；默认分支 push、tag、`schedule`、`workflow_dispatch` 一律全跑，只在 PR 上跳；workflow、本地 action、配置文件变了就全跑；任何不确定一律全跑；有 `outputs`、上传 artifact、`environment`、`container`/`services` 的 job 不跳；提供手动覆盖（PR 标签或 `[ci full]`）；"审计模式"：被判可跳的 job 偶尔真的跑，统计漏判率。**先做的不是实现，而是用真实仓库的历史做离线回放**：最近若干提交里按规则会跳过多少 job，对照这些 job 当时实际是否成功，看漏判率；漏判率不能接近零就不做。只对 monorepo 里常常不受影响的长 job 有用，单 job 仓库（基准负载）没用。

**Node 家族专项（用户 2026-10-31 大致同意；A、B 两条路径都能用）**：Vercel 的按需构建是 **Turborepo**（任务级内容哈希缓存，输入没变就重放输出和日志，`--affected` 只跑受影响的包）；**Turbopack** 是 Next.js 里的增量打包器（进程内函数级缓存），两者思路相同、粒度不同。原则：**让用户自己的工具按内容哈希来跳过，我们负责把缓存放在近处**，正确性由工具保证，不会有我们猜依赖导致的假绿。按把握从大到小：
- ⬜ **注入 Turborepo / Nx 的远程缓存**：据我所知两者都有公开的 HTTP 接口、可自托管（**没核对细节**）；用我们的对象存储（`internal/blob`）写一个小服务，用 runner 目录里的 `.env` 注入 `TURBO_API`/`TURBO_TOKEN` 等变量，用户不用改 workflow。Actions 和自己的 trigger 两种路径都能用。验证办法：`create-turbo` 生成真实项目，写最小的缓存服务，跑两次 `turbo run build`，看是否命中、重放、省多少。风险最大的一项，因为依赖对接口的记忆
- ⬜ **把 Node 和包管理器预先放进工具缓存**：数据里 `typescript-monorepo` warm 中 `pnpm/action-setup` 6 s + `setup-node` 7–8 s ≈ 14 s，占 52–67 s 的两成多；`setup-node` 在工具缓存里找到版本就不下载。排队时本来就会读 workflow，用现有的 `scan` 和 `provision` 在 runner 接 job 之前备好版本（窗口约 3–4 s，要预先把包放在机器上）
- ⬜ **热盘保留框架的构建缓存**：`.next/cache`、`.turbo`、`node_modules/.cache`、tsc 增量信息、eslint 和 jest 缓存；Turbopack 的磁盘缓存位置待确认。属于环境快照（sticky disk）那一类
- ⬜ **预装重型二进制**：Playwright 浏览器、Cypress、`postinstall` 下载的原生模块；`frontend-e2e` 里 `npx playwright install` 每次都做，占多少还没量
- ✅ 前后端分离的并行：本来就是不同 job、各自一台机器
- 先做的两个小验证：①用现有步骤耗时数据算出 Node 类 job 里"装环境"类步骤占多少（第 2、4 项的天花板，几乎不花钱）；②Turborepo 远程缓存的最小实验

**任务图**（读 workflow 源文件，见 A.1 的提前启动）：目前只用在"下游 job 的提前启动"。读到的静态信息还能：一次得到所有 job、`needs`、矩阵个数、`runs-on` 标签；用 `plan` 求值静态的 `if:`，为假的 job 不起机器；在 push/PR 事件到达时一次性提前起所有第一层 job（需要订阅 `push` 事件）；同一个 run 里让下游 job 接着在上一个 job 的机器上跑（热的包管理器 store 和工具缓存，限同一个 run）；轮询上游 job 的步骤进度（`GET jobs/{id}` 带 `steps`）来修正下游启动时间（**已做**，`progress.go`，2026-11-01 真机验证：一个带 5–24 s 随机步骤的 build 加一个 deploy，8 次预测，按步骤的平均绝对误差 5.3 s、只按中位数 5.0 s，但按步骤从未偏晚（误差 −9 到 −2 s），只按中位数有 4/8 偏晚（最晚 +9 s，runner 约 7 s 才能启动，所以是冷启动）；有历史后下游 job 都是 1 s 开始。样本只有 8 次，而且是故意设计出大方差的情形，真实 workflow 差距会小）。**矩阵个数**（**已做**，`matrix.go`）：静态矩阵直接从 workflow 数（受 `max-parallel` 和 `MaxJobs` 限制），含 `${{ }}` 的动态矩阵用同一个 job 在最近 5 次里展开个数的**最大值**（不是中位数：多起一台等待 runner 约花 15 s 机器费，少起一台让一个 job 多等约 7 s 冷启动，代价差几百倍）。真机（2026-11-01）：中位数版本 8 次（个数 2 4 3 2 2 5 5 4），25 个有历史的 job 里 8 个走了冷启动、浪费 2 台；最大值版本每 100 s 一个 run、8 次，有历史的 7 个 run 共 28 个 job 里只有 1 个冷启动（历史最大 4、这次 5）、浪费 5 台。**但名额紧张时没有用**：每 62 s 一个 run（每个 run 5–6 次起机，对每分钟 10 台）时，限额器挡下了一部分提前起的 runner（以前这条路径不打日志，现在有 "N of those M runner(s) not started"），最大值版本和中位数版本一样约三成 job 冷启动。所以这是起机名额的问题，不是算法的问题：一个仓库每分钟一个大 fan-out 就会顶到 Pro 套餐的上限。可能的出路：同一个 run 内复用机器（给空闲的机器换一份新的 JIT 配置，不占起机名额；代价是同一个 run 的 job 之间不再隔离，要慎重）、更高的套餐、或者把名额预留比例调整。**起机限额**（**已做**，`starts.go`）：InstaBox Pro 每分钟最多起 10 台，`-starts-per-minute`；之前撞到 429 三次就永久放弃 job。**runner 可互换**（**已做**）：runner 会去跑最老的排队 job 而不是为它启动的那个，这个 job 之后没有 runner 就会被遗忘，现在 runner 走后若 job 还在排队就再要一个。三个都是真机实验发现的缺陷，各有一个去掉修复就会失败的测试。**读错 workflow 版本的缺口**（**已修**，2026-11-01，已提交，`workflowOfRun`）：`pull_request` 的 run 用合并提交上的 workflow 文件，`pull_request_target` 和 `workflow_run` 用基础分支或默认分支的，都不是 run 的 `head_sha`；现在这几类事件按 run 的 `path` 里 `@` 后面的 ref 读（读不到再退回提交号），其余事件仍按提交号。顺带修了 `workflowFile` 对分支和 tag 永久缓存的问题（`refreshSchedules` 读的是默认分支，所以 `main` 上改了 cron 永远发现不了），现在不是完整提交号的 ref 只缓存 5 分钟。四类事件的测试在去掉修复时会失败。**没有在真 PR 上验证 run 的 `path` 的实际内容**（凭对 GitHub 行为的了解，测试里用的是假的）。**任务图里还没做的**：静态求值 job 的 `if:`，为假的 job 不起机器；同一个 run 里让下游 job 接着在上一个 job 的机器上跑；边车（从机器上读上游 job 的真实输出）。

---

# B. 作为独立 CI/CD 平台（`raccoonci run`，自有引擎）

Raccoon CI 自己执行 workflow，所以要自己实现 GitHub runner 和 GitHub 服务端本来提供的东西：解析、求值、装环境、执行、artifact/cache/OIDC 等运行时服务、job 类型、workflow 语义。长期运行的服务和算力见 C。

## B.1 已完成

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

## B.2 装环境——剩余部分

Node.js、Go、Python、Java、Rust、apt 包已经通了（见 B.1 和下面的条目）。2026-11 收尾：运行时兜底和下载镜像已做，其余没做的条目移到本节末尾的"搁置"，不再算 B.2 的未完成项。

- ✅ Java（Temurin，Adoptium 接口）和 Rust（static.rust-lang.org 的 rustc/cargo/rust-std/rustfmt/clippy 组件包，逐个校验 SHA-256 后用各自的 install.sh 装）的安装器（2026-10-29）。两者在 catalog 里标了 `implicit-only`：`setup-java`、rustup 自己会下载，预填 tool cache 它们不会读，所以只给“没写 setup 就直接用 `cargo`/`java`/`mvn`”的 job 装。真实网络实测：一个 job 隐式用 cargo、rustc、cargo fmt、java，11 s 装好。Rust 只支持 `stable` 和精确版本（1.80、1.80.0），Java 只支持主版本和精确版本（17、17.0.9、1.8），范围写法（`>=1.70`、`lts`）不支持；没有探测机器自带的 Rust 和 Java，总是下载
- ✅ 采用机器自带的工具链：InstaBox 的 `universal` 模板自带 Go、Node 等。装环境前先按 catalog 的 `probe`（运行什么命令、从输出哪一段取版本号）探测，版本满足就把安装目录链接进 toolcache，不再下载。运行时探测，不写死模板里有什么。实测：全新缓存目录下跑自己的 CI，`go 1.27.1 (already on this machine)`，`setup-go` 直接 Found in cache。目前只给 node 和 go 配了 probe；Python 不采用系统版本（`setup-python` 要的布局不同），其余工具链（Rust、Java 等）还没有安装器也没有 probe
- 🟡 项目环境快照（装完依赖拍快照，锁文件不变就复用）：键和登记表已做（`internal/envsnap`）。键 = 锁文件内容哈希 + 用到的工具链名 + 机器镜像标识，不含工具链版本；`plan` 会显示每个实例能否快照。真实项目上：cobra/本项目（go.sum）、commander.js（package-lock.json）、click（uv.lock）可以；express 没有锁文件，判定为不能快照。**已接到 `-on instabox`（`-env-snapshots N`，默认关）**：实例启动前按键查登记表，命中就从该快照建机器，快照已被删（400/404）则忘掉并退回 runner 快照重跑；未命中时，成功的一次运行会在后台清理机器（删 agent 配置和本次 workspace，保留各种缓存）、拍快照、再删机器，同一个键同时只有一台机器拍；超出上限的旧快照由 API 删除。只在可信事件（非 `pull_request*`）、且没有 `-secret` 时才拍。**monorepo**：键只含仓库根的锁文件，所以用到的工具链在子目录里有清单或锁文件（跳过 `.git`、`node_modules`、`vendor` 等）时，判定为不能快照（`ErrUnpinned`），避免子目录依赖变了却复用旧快照；把子目录锁文件也算进键还没做。**还没做**：**真实 InstaBox 端到端已验证（2026-10-29）**：带 uuid 依赖的小 Go 项目，三次运行：①未命中，建机器 10.8 s，构建 4.2 s（下载依赖），成功后保存快照；②命中，机器 7.7 s，构建 0.3 s，整条命令 35 s → 10.6 s；③改 go.sum 后键变了，未命中并保存新快照，旧快照被淘汰（上限 1）。测试机器自动删除，测试快照和端口已手工清理。**另一个实测发现（本项目 ci.yml，`-on instabox`）**：`actions/setup-go` 的缓存（747 MB，含 go-build）从主控恢复用了 2m26s，比什么都不缓存的冷跑（总共 1m45s）还慢——主控（这台 InstaBox 机器）出口约 6 MB/s。所以在"主控本身在 InstaBox 上"的开发配置里，开着环境快照时 `setup-go` 的缓存恢复反而拖慢整个 job；正式的主控要放在带宽足够的地方，或把缓存对象放对象存储，机器直接从存储下载。**缓存对象放 S3 已做（2026-10-29）**：`run -cache-s3-bucket B [-cache-s3-endpoint URL -cache-s3-path-style -cache-s3-region R -cache-s3-prefix P]`，密钥只从环境变量 `RACCOONCI_CACHE_S3_KEY`/`_SECRET`（可选 `_TOKEN`）读；每个仓库一个子目录；下载是预签名链接，机器直接找存储；上传仍经过主控（缓存协议是对 Azure Blob 的模拟，直传要另改）。用本地 SeaweedFS 的 S3 网关验证了功能：1 GB 的 setup-go 缓存保存成功、第二次运行能找到并下载。**没有在真实外部桶上量过速度**——本地 SeaweedFS 和主控在同一台机器上，量不出带宽差别。在 InstaBox 机器上实测：入站从公共 CDN 下载 78 MB 用 0.42 s（约 185 MB/s），出站约 6 MB/s 且是每台机器各自的限制（两台同时传互不影响，同机两个并发上传共享这 6 MB/s），所以缓存下载应该会快很多，保存缓存（上传）不会。验证中发现并修了一个真问题：S3 的分片是流（`LimitReader`），SDK 重试时倒不了带（"request stream is not seekable"），存储偶尔 5xx 就让保存直接失败；现在不超过 32 MiB 的分片先读进内存再发（`maxBufferedPart`）
- ✅ 通用 runner 快照（InstaBox）：`universal` 模板（Debian 13）+ 静态编译的 `raccoonci`（`/usr/local/bin`）+ 清单 `/etc/raccoonci-runner.json`，不含工具链、依赖、机密、登录信息。做法：新建一台干净的 `universal`，临时用 `instabox-port serve` 发布只含二进制的目录（随机路径），机器上下载并校验 SHA-256 后立刻撤下端口，检查母机无凭据文件后拍快照。实测：从快照建机器约 9.5 s，在上面用快照里的 `raccoonci` 跑 checkout + `setup-go` + `setup-node` + 运行步骤，7.2 s 成功，Go 和 Node 都直接采用机器自带的版本。这次验证还发现并修了一个真问题：模板把 Go 和 Node 装在同一个 `/usr/local/bin`，采用时不能把整个前缀链接进某个工具链的目录，否则会把别的工具带上 PATH，盖掉前面步骤选的版本（已修，见 `sharesPrefix`）。**更新方式还是手工的**（每次新版本要重做快照）；要有自动化，并在快照清单里校验 `raccoonci` 版本和模板版本（后来做了 `raccoonci runner-snapshot`，见 C.2）
- ✅ 第三方 action 内部的依赖（2026-10-29）：扫描会下载 catalog 里没有专门规则的 action 的 `action.yml`（`scan.Input.Action`，引擎里用 `actions.Store.Lookup`，和 runner 共用同一个 Store，所以不多下载），docker action 记 docker；composite action 递归扫它的步骤（最深 4 层，自己调自己会停），内部的 `setup-*`、`run:` 里的命令都按普通步骤算；`inputs` 用调用方的 `with`，没有就用 action 的默认值，运行时才知道的输入不会漏出占位符当版本；内部步骤的 `if:` 能判断为假就跳过（`runner.os` 当作 Linux，所以只给 Windows/macOS 的步骤不会被当成需求），判断不了就保留。仓库里的本地 composite action（`./path`）也读 `action.yml` 扫。`raccoonci plan -fetch-actions` 可以在不运行时看到这些（默认不联网）。在 80 个真实 workflow 上对比：13 个多了新需求，例子：`ossf/scorecard-action` 是 docker action；`prometheus/promci/build` → `promci-setup` 两层 composite 里要 go、make、docker；`golang/govulncheck-action` 内部的 `setup-go stable`；`curl`、`gh`、`pwsh`。**还没做**：外层 workflow 自己步骤的 `if:`（比如 `runner.os`、matrix 条件）也还没用来跳过不会运行的步骤；`-on instabox` 时主控这边没有 Store，所以环境快照的键不含 action 内部的工具链（机器那边的扫描是完整的）；JS action 的 `package.json` 依赖不看（它们自带 node_modules）；docker action 的 `Dockerfile` 内容不看
- 🟡 工具链缓存清理（2026-10-29）：`raccoonci run -toolcache-limit-gb N`（默认 10，0 为不限），本机运行开始时按“最久没用”淘汰版本，直到不超上限；24 小时内用过的不删，所以上限是软的。“最近使用”记在 `.complete` 标记的修改时间上，每次选中某个版本都刷新；超过 24 小时的中断安装残留（`.part-*` 等）顺手清掉；采用的机器自带版本只是一串链接，不计大小。工具缓存在用户缓存目录（InstaBox 上是 `/data/home/.cache`，持久盘），不在 `/tmp`。**没做**：远程模式下机器上的缓存不清理（每台机器用完即删）；`node/`（Node.js 运行时，实测 750 MB）和 `runs/`（工作目录，765 MB，只增不减）不在这个上限里；只在运行开始时清一次，一次运行里装的东西不会在运行中被淘汰；setup-* 动作自己写进工具缓存的版本也会被淘汰，下次由它们重新下载

**搁置（不再算 B.2 的未完成项）**：下面几条只在自有引擎跑在非 Ubuntu 机器（Debian、用户自己的机器）时才有意义。InstaBox 的 `ci` 模板是 Ubuntu 24.04 加 GitHub 的 runner 镜像，`serve` 路径用它；用户认为自有引擎也能跑在 `ci` 模板上（**未验证**：当前 runner 快照由 `universal`（Debian 13）做成，`raccoonci runner-snapshot` 写死了这一点，且 catalog 里的 Debian 13 平台、`lsb_release`/PPA 警告和 `probe` 采用自带 Go/Node 的做法在 Ubuntu 上都要重新看；这件事本身放在 C.2）。**`ci` 模板的预装内容已实测（2026-11，起一台机器查完即删）**：Ubuntu 24.04.5，x86_64，glibc 2.39，`RUNNER_TOOL_CACHE=/opt/hostedtoolcache`。tool cache 里有 Go 1.24.13/1.25.14/1.26.10、Node 22.23.4/24.22.1、Python 3.11.18/3.12.16/3.13.18/3.14.10/3.15.2、PyPy、Ruby 3.2.11/3.3.13/3.4.12/4.0.9、Temurin 8/11/17/21/25；PATH 上有 ruby 3.2.3、gem、node 22、go 1.24、java 17、mvn、gradle、cargo/rustc、gh、kubectl、helm、yarn、docker、jq、git、gcc、make。**没有**：terraform、`bundle`（`gem install bundler` 才有）、`python3.10`（tool cache 里最老的 Python 是 3.11，`setup-python` 在 Ubuntu 上会按 GitHub 的清单下载 3.10）、`ruby-build`、`raccoonci`。所以 Ruby 和 CLI 工具在这个模板上基本不用装，但 terraform 和旧版 Python 要靠 `setup-*` 或下载。

- ⬜ Ruby 的安装器。`ruby/setup-ruby` 只认 `ubuntu-22.04/24.04/26.04`、macOS、Windows（读过它的 `common.js`）；Debian 被当成自托管，只去 `$RUNNER_TOOL_CACHE/Ruby/<版本>/<arch>` 找，没有就报错让用户自己 `ruby-build`。要在 Debian 上支持，得预填 tool cache：`ruby-build` 编译（每个版本几分钟，要 gcc、libssl-dev、libyaml-dev）或试 ruby-builder 的 Ubuntu 预编译包（没试过，包里可能写死了 `/opt/hostedtoolcache`）
- ⬜ 旧版 Python（3.10 及更早、或旧补丁）：最新一期 python-build-standalone 不含，要翻更早的发布。**不必走 GitHub API**：下载地址 `releases/download/<tag>/…` 本来不走 API，tag 列表可用 `git ls-remote --tags` 或 Atom feed；带 token 时 API 限额是 5000 次/小时（沙箱里的 gh 已登录，实测剩余 4931），只有无 token 的用户机器才是 60 次/小时
- ⬜ 各种 CLI 工具的安装器：docker、gh、kubectl、helm、terraform、yarn、maven 等。目前只检查是否已在 PATH 上，缺了就警告
- ⬜ 其他架构：toolcache 的 arch 目录已按机器架构命名，但只在 x64 上实测过；没有 arm64 机器可测

## B.3 运行时服务（GHA action 依赖的内部接口）

- ✅ artifact v4（Twirp + Azure Blob 的 Put Block / Put Block List）：`internal/artifact`、`internal/blob`（本地目录和 S3 兼容存储）。`raccoonci run` 里 `upload-artifact@v4` 与 `download-artifact@v4` 跨 job 实测通过（20 MB，sha256 一致）
- ✅ 按 job 签发 token：`-on instabox` 下每个实例一个 token（`services.jobEnv`，scope 里的 job id 是 `<job>-<n>`），只能以自己的 job 创建 artifact，别的 job 的 artifact 仍可读（和 GitHub 一样）。`remote_test.go` 有单测。**没做**：本机 `raccoonci run` 仍是整个 run 共用一个 token
- ⬜ artifact 剩余（搁置，见本节末尾；元数据持久化和 retention 清理服务端已做，见 C.1 的 artifact 一条）：整体 sha256 校验（现在只记录）；在真实 AWS 上验证 S3（目前只测过 SeaweedFS）；`download-artifact` 的按 ID 跨 run 下载（需要 GitHub token）
- 🚫 artifact v3（REST）：不做。GitHub 已弃用 v3，用 v3 的 workflow 应该升级到 v4
- ✅ cache v1（`/_apis/artifactcache`）：`internal/cache`。真实 `actions/cache@v4` 与 `cache/restore@v4` 实测（20 MB、1 MiB 分块、两个 job；第二个进程命中第一个进程存的缓存）。缓存按仓库放在用户缓存目录，跨次运行保留
- ✅ cache 淘汰与大小上限：`-cache-limit-gb`（默认 10，和 GitHub 一样，0 不限）超了就淘汰最久没用的，单个条目超过上限直接拒绝；提交前按 `statfs` 检查磁盘剩余，不够就先淘汰、仍不够返回 507。`internal/cache/limit_test.go` 有 v1/v2 的单测
- ✅ cache 放 S3 兼容存储：`-cache-s3-bucket`（凭据走 `RACCOONCI_CACHE_S3_KEY/_SECRET/_TOKEN`），条目放桶里，客户端从桶的预签名链接下载。对着 SeaweedFS 的 S3 网关验证过，没有试过真实的外部桶
- ⬜ cache 剩余（搁置，见本节末尾）：按分支隔离（现在同仓库所有 ref 共用）；多个 `raccoonci` 进程同时写同一个 index（后写覆盖）；下载链接与 run 共用同一把密钥
- ✅ cache v2（Twirp `CacheService`：CreateCacheEntry / FinalizeCacheEntryUpload / GetCacheEntryDownloadURL）：`internal/cache/v2.go`，上传那一半抽成了 `internal/azblob`（新增单次 Put Blob）。`raccoonci run -cache-protocol v2` 下用真实 `actions/cache@v4` 实测：20 MB（单次 Put Blob，第二个进程命中）、150 MB（超过 SDK 单次上限 128 MiB，走分块），各跑两个 job。v1 与 v2 共用同一批缓存条目
- ⬜ cache v2 剩余：默认协议仍是 v1（toolkit 只有设了 `ACTIONS_CACHE_SERVICE_V2` 才走 v2，服务端不会按 toolkit 版本自动选）；请求里的 `CacheMetadata`（repository_id、scope）被忽略；150 MB 那次没有确认分块走的是直通路径还是落盘路径
- 🟡 OIDC token 端点（2026-11-02，**未提交**，服务端 `internal/server/oidc`）：**设计取舍，先按最稳妥的做，等你拍板**——①签发者是服务端自己（`<public-url>/oidc`，所以每个云账号要为这个签发者设一次信任；而不是接外部身份）；②RS256，密钥用主密钥封存存库（没有主密钥则每次启动一把新的，验证方重新取公钥集即可）；③request token 是进程内随机密钥的 HMAC，绑定 run+job、run 结束后失效，重启后随 run 重排队而重发；④只有 job（或 workflow）有 `id-token: write` 的才给 `ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN`，fork 的 PR 永远不给；⑤claim 用 GitHub 的名字和 `sub` 形式（`repo:o/r:ref:<ref>` / `repo:o/r:pull_request`），默认 `aud` 是 `https://github.com/<owner>`。端点：`/oidc/.well-known/openid-configuration`、`/oidc/jwks`、`/oidc/token`。单测：oidc 包 8 个（发现文档、用公布的公钥集独立验签、claim 逐项、`sub` 各形式、request token 的各种拒绝：缺失/乱码/改签名/改负载/别的进程/过期/没有的 run/已结束的 run、audience 过长或带换行、密钥封存且重启后不变、主密钥换了就换新的、无 vault 时每进程一把、要求 https）、执行器 2 个（哪些 job 拿到环境、fork 不给）；真 runner 的测试（`TestRealRunnerAJobFetchesItsIDToken`：step 用 curl 按 toolkit 的方式取 token，claim 正确）；经隧道取到了发现文档和公钥集。**没做/没测**：没有任何云厂商接受过这种 token（需要用户在某个云账号里建信任）；`environment` 和带它的 `sub`、`head_ref`/`base_ref`、`repository_id`、`job_workflow_sha`、自定义 `sub` 模板；`actions/core` 的 `getIDToken` 没在真机上跑过（那些仓库没有 workflow 可加）；`ghrunner`（GitHub 触发路径）不需要这个，GitHub 自己签
- 🟡 真实的 `GITHUB_TOKEN`（2026-11-02，**未提交**，服务端 App 模式）：每个 job 实例一个 installation token，只对 run 的仓库，权限取 job 的 `permissions:`、否则 workflow 的、否则默认（`internal/server/execute/permissions.go`，`github.App.RunToken`）；`-job-token` 默认开。**设计取舍，先按最稳妥的做，等你拍板**：①默认只读 contents+metadata（GitHub 自己的默认可能读写，取决于仓库设置，所以依赖默认写权限的 workflow 需要显式写 `permissions:`）；②fork 的 PR 一个 token 都不给（GitHub 给只读的；我们跟"不给密钥"一致）；③App 没有 job 要的权限时降级为默认权限并在日志警告，而不是让 job 失败；④普通 token（开发用）绝不传给 job。单测：权限表（映射、空映射、read-all、write-all、job 覆盖 workflow、去掉 id-token）、客户端 2 个（只问一个仓库+指定权限、422 与无 installation）、执行器 4 个（有 token、fork/无 installation/无 minter 没有、拒绝时降级并警告、全失败时无 token 并警告、日志里没有 token）。**真 GitHub 上测过 GitHub 一侧**（2026-11-02，`TestRealAppTokens`，用 `RACCOONCI_TEST_APP_*` 开启）：只对一个仓库的 token 能读它（200）、读同一安装下别的私有仓库 404；只读 token 写 status 403；`statuses: write` 的能写（201）、读 contents 被拒；App 没有的权限（packages）真的返回 422，所以降级路径是对的；吊销后失效（GitHub 要等一小会儿才生效，测试轮询）。一个真实 run 里要 `packages: write` 的 job 拿到降级 token 并有警告。用真正的 `run-instance`（`TestRealRunnerGivesAJobItsGithubTokenAndMasksIt`，`RACCOONCI_TEST_BIN`）也测了：step 里 `github.token` 和 `secrets.GITHUB_TOKEN` 是同一个值，打印出来是 `***`，日志里没有原值。**没测到的**：InstaBox 机器上的 step 真的拿真 token 去调 API——手边的 gh token 没有 `workflow` 权限，加不了 workflow 文件，把 run 里存的文本换成探针也没用（**发现：机器从源码包里读 workflow 文件，不是用库里保存的文本，两者在同一提交上一致所以平时没事**）。要测的话需要你运行 `gh auth refresh -s workflow`（或在 `bench-lint-docs` 里加一个探针 workflow）。没做：`id-token`（OIDC，仍是 B.3 的未决项）；（job 一结束就吊销它的 token：`App.RevokeToken` = `DELETE /installation/token`，用 token 自己认证，已失效的 401 算成功，取消时也会吊销，失败只记日志、一小时后自己过期；单测：客户端 1 个、执行器断言吊销了正确的 token）
- ⬜ job summary 的展示与 annotations 持久化：`runner.Result.Summary` 收集了 `$GITHUB_STEP_SUMMARY` 的内容（上限 1 MiB），但没有任何地方读它，本机运行也不打印。用户在自有引擎下看不到 summary

**搁置：单机之外才需要**：（artifact 元数据持久化和 retention 清理服务端已做，`raccoonci run` 本机仍不做）整体 sha256 校验、cache 按分支隔离、多进程写同一个 index、下载链接单独的密钥、本机运行的按 job token。它们都只在多人共用一台 controller 或长期运行的服务里才会出问题；现在 `raccoonci run` 是单次运行、单进程。做长期服务（C.1）时再捡起来。

**B.3 的未决项（需要你定）**：OIDC 和真实的 `GITHUB_TOKEN` 是 GHA 兼容性缺口，用 OIDC 登录云厂商的 workflow 现在会失败。做法取决于谁来签发 token（自有引擎自己签，还是接外部身份），还没定，所以没有排期；定了以后需要服务端（C.1）。

## B.4 job 类型缺口（现在会给出明确报错，不会静默）

- ✅ 容器 job（`container:`）和 `services:`：`internal/docker`、`internal/runner/containers.go`。用真实 Docker 实测（`RACCOONCI_TEST_DOCKER=1`）：busybox 和 debian:bookworm-slim 作 job 容器、redis 作 service（健康检查、容器内按名字访问、宿主机 job 通过发布的端口访问）、容器内的本地 Node action、job 失败/镜像不存在/service 不健康时的清理，结束后没有残留容器和网络
- ✅ 容器 job 里的 artifact 和 cache 服务：job 网络建好后，服务在这张网络的网关地址（宿主机在这张网络上的地址）上再开一个监听，只接受这张网络子网内的连接；签发的上传/下载链接按请求进来的那个地址生成（`blob.WithBaseURL`），鉴权不变。实测：容器里 `upload-artifact@v4`（12 MB）+ `actions/cache@v4`（v1 和 v2 都测了），之后容器 job 和宿主机 job 各自下载并校验通过；其他 docker 网络和宿主机 eth0 地址连不上，同一网络里没有 token 返回 401；跑完后监听、网络、容器都没有残留。rootless docker 下不支持（网络不在宿主机上），会给出警告
- ✅ 取消容器里的步骤：步骤经 `sh -c 'echo $$ >pidfile; exec …'` 启动（docker 把每个 exec 放进自己的进程组，pid 即 pgid），取消时用另一次 `docker exec` 向容器内的进程组发 SIGINT，宽限期后发 SIGKILL。实测：步骤里的 `trap INT` 会执行，后台子进程随组退出；去掉这处修复后该测试会失败
- ✅ `docker login` 对真实私有仓库：本地 `registry:2` + htpasswd 认证，job 用 `credentials:` 拉取成功；密码错误则 job 失败且日志里没有密码；宿主机自己的 docker 配置没有被登录状态污染
- ✅ job 结束后 chown：断言容器里 root 写的文件归还给当前用户；把 chown 关掉后断言会失败
- ✅ Alpine（musl）镜像的 JS action：job 容器是 musl 时用 `unofficial-builds.nodejs.org` 的 musl 版 Node（同样校验 SHA-256，装在 `node/musl/` 下，随 `node/` 一起挂进容器）。musl 版动态链接 libstdc++，Alpine 没有，所以 Node 起不来时在容器里以 root 执行 `apk add libstdc++`（只影响这个 job 的容器，步骤日志有 notice）。实测真实 `upload-artifact@v4`/`download-artifact@v4` 在 Alpine 容器里跑通。`actions/cache` 在裸 Alpine 里会因 busybox 的 tar 失败，与 GitHub 上相同，镜像里需要 `apk add tar`
- ⬜ 容器 job 剩余：宿主机防火墙若拦截 INPUT（如 ufw 默认 deny），容器连网关会失败，目前没有检测；非 root 用户的镜像（`--user`）；被强杀的进程留下的孤儿容器没有清扫（容器带标签 `raccoonci=1`）；Alpine 的 `apk add` 需要容器能访问 Alpine 镜像源，离线或非 root 的 Alpine 镜像要自己预装 libstdc++；没有 `/bin/sh` 的镜像（distroless）上取消仍然只会杀 docker 客户端
- ✅ Docker action（2026-10-29，`internal/runner/docker_action.go`）：`uses: docker://image`（`with.entrypoint`、`with.args` 按命令行规则切分，不展开）和 `runs.using: docker`（`image: docker://…` 先拉取，`image: Dockerfile` 用 action 目录 `docker build`，每个 job 只建一次；`runs.entrypoint`/`args`/`env`，args 和 env 里可以用 `inputs`；`pre-entrypoint`/`post-entrypoint` 带 pre/post-if）。做法照 GitHub 的 runner：`docker run --rm`，工作区、temp、工具缓存、文件命令目录、event 目录和一个 home 分别挂到 `/github/workspace`、`/github/runner_temp`、`/opt/hostedtoolcache`、`/github/file_commands`、`/github/workflow`、`/github/home`，`GITHUB_*`/`RUNNER_*` 里的路径变量改写成容器里的路径，不传本机环境和 PATH，有 job 网络就加入，挂 docker.sock；输出走和别的步骤一样的路径，所以 `::set-output` 类命令和 `$GITHUB_OUTPUT`/`$GITHUB_ENV` 都能用；取消时先 SIGINT（docker 客户端转发给容器），宽限期后 `docker kill`。结束后用同一个镜像 `chown` 把容器里 root 写的文件还给当前用户（镜像没有 chown 就算了）。真实验证：`actions/hello-world-docker-action@v2`（Dockerfile 构建，输出被后面的步骤读到）、`docker://alpine`，以及一个带 pre/post/args/env 的本地 Dockerfile action（`TestRealDockerActions`，需要 `RACCOONCI_TEST_DOCKER=1`）。**注意**：`runs.args` 现在也传给 pre/post entrypoint（凭记忆照 runner 的实现，没找到文档确认）；pre 只对远程 action 执行（和 JS 的 pre 一样）；不支持 `--user`/job 容器 `options:` 对 action 的影响；机器上 docker 要可用（InstaBox 的 universal 模板有）
- ⬜ 可复用 workflow（`jobs.<id>.uses`、`workflow_call`）
- ⬜ problem matcher（目前输出原样记录，不生成 annotation）

## B.5 workflow 语义缺口

- ✅ `on:` 触发匹配（2026-10-29，`internal/trigger`）：事件写法三种（字符串、列表、映射）；`push`/`pull_request`/`pull_request_target` 的 `branches`、`branches-ignore`、`tags`、`tags-ignore`、`paths`、`paths-ignore` 按 GitHub 的规则——过滤用的是 GitHub 自己的 glob（`*` 不跨 `/`，`**` 跨，`**/x` 也匹配根目录的 x，`?` `+` 作用于前一个字符，`[]` 字符集，`\` 转义），列表里 `!` 取反且顺序有意义（后面的正向模式可以重新选中），只有取反模式的列表报错；只写了 branches 时推 tag 不触发、只写了 tags 时推分支不触发；PR 的 branches 过滤看目标分支；tag 推送不看 paths；`paths` 与 `paths-ignore`、`branches` 与 `branches-ignore`、`tags` 与 `tags-ignore` 同时出现是错误；`types`（`pull_request` 默认 opened/synchronize/reopened，其它事件不写 types 就全部）；`schedule` 校验 cron 是 5 段并按字符串匹配触发的那条；`workflow_dispatch` 的 `inputs` 解析（默认值、required、choice 的 options、boolean/number 的类型转换、不认识的输入报错——`run -event workflow_dispatch -input k=v` 现在走这条，`inputs.x` 因此是带类型的，和 GitHub 一致）。文档里的过滤示例做成了测试，80 个真实 workflow 全部能解析。命令行：`raccoonci match -event E [-ref R -base-ref B -before REV -file F -action A -cron C] workflow.yml`（退出码 1 表示不触发；变更文件默认取 git 的 `HEAD~1..HEAD`，PR 取 `base...HEAD`，取不到就是"未知"，paths 过滤不拦），`run -only-if-triggered` 同样判断后才跑（默认 `run` 仍然假设触发，因为是用户明确要跑）。`serve` 路径的 push/PR 提前启动也用它（A.1）。**还没做**：`workflow_call`（可复用 workflow，见 B.4）；`workflow_run`、`merge_group` 的 `workflows`/`branches` 过滤；push 事件按 GitHub 的"最多 300 个文件就总是触发"；`schedule` 的 cron 到点计算（自有引擎的服务端要做；`serve` 路径已有 `cron.go`）；把 `match` 的事件从 webhook 载荷里取（服务端，C.1）
- 🟡 `concurrency`（2026-11-02，**未提交**，服务端 `execute/concurrency.go`）：workflow 级 `concurrency:` 按仓库+组名（不分大小写）生效，组名和 `cancel-in-progress` 都可以是表达式（`github`/`inputs`/`vars`）；组里一个在跑、一个在等，新来的顶掉等待者（等待者以 cancelled 结束，日志写明），`cancel-in-progress` 为真时立刻取消在跑的；等待中被取消会让出位置；源码在等待之后才取。单测 7 个（排队等待、只留最新的等待者、cancel-in-progress、它是表达式且为假时只等、不同组并行、取消等待者后组空出、组名表达式出错），`-race` 重复 5 次稳定。**job 级 `concurrency:`（2026-11-02，**未提交**）**：同一套组机制（`holder` 带自己的取消函数，run 和 job 共用）；job 的组表达式能用 github/inputs/vars/needs/strategy/matrix；进组在 StartJob 之前，等待的 job 没有行、check run 和机器；`cancel-in-progress` 只取消组里的那个 job（它以 cancelled 结束，日志写明），不取消它的整个 run；被顶掉的等待 job 结论为 cancelled；workflow 级与 job 级的同名组是两个组（键带层级前缀）；等待 job 的 run 仍占着 `MaxRuns` 名额（run 已在执行，不让出）。单测 7 个（跨 run 等待且无行、只留最新的等待者、cancel-in-progress 只取消 job、同一 matrix 同组轮流、每个值一个组并行、两层同名是两个组、组名表达式出错）；变异检查：去掉 job 级进组时 4 个测试失败。**取舍/没做**：一个被取消的 job 让所在 run 的结论是什么，由引擎按 needs 的规则决定，没有单独调整；**没做**：`concurrency` 在 `raccoonci run`（本地引擎）里没意义所以不做；`raccoonci run`（本地引擎）里的 `concurrency` 没有意义所以没做；状态只在内存里（单实例）；（等待组的 run 不再占 `MaxRuns` 名额：等待时让出、轮到时再拿，让出时唤醒执行器去领别的 run；测试 `MaxRuns=2`，一个在跑一个在等时另一个组的 run 照常跑完，变异检查：去掉让出这个测试会失败）；没在真机上测
- ⬜ `permissions` 和 `environment`（含保护规则、环境级 secrets）
- ⬜ `run-name`
- 🟡 `github` 上下文：`run` 命令从 git 生成了一部分字段，真实的 run_id、事件载荷等要服务端提供（C.1）
- 🟡 `hashFiles`：自己写的 glob 实现，**没有逐项对照 `@actions/glob`**，边界行为可能有差异

## B.6 按需执行（Raccoon CI 自己的扩展，不是 GitHub 的语义）

灵感来自 Vercel / Turborepo：改了后端就不跑前端的 job。`internal/affected` + `engine.Options.Unaffected`。这是自有 trigger 才能做的加速；A 路径下怎么用见 A.4 的"跑了但实际没跑"。

- ✅ job 级（2026-10-29）：`raccoonci affected [-event E -file F -before REV ...] workflow.yml` 说明每个 job 为什么跑、为什么不跑；`raccoonci run -affected` 把不受影响的 job 直接略过（打印 "not run: …"，最后汇总里标 `-`），依赖它的 job 看到的 `needs.<id>.result` 是 `success`。判断依据保守：只有**能确定** job 的输入范围，并且变更文件都落在范围外，才会略过；其余一律照跑。范围来源：①job 的 `defaults.run.working-directory` 或每个 `run:` 步骤自己的 `working-directory`，加上 `uses: ./本地action` 的目录（任何一个 `run:` 在仓库根目录跑、目录只在运行时才知道，都当成"整个仓库"）；②包依赖图（下一条）；③可选的 `.raccoonci/affected.yml`（`ignore` 不影响任何目录型 job 的文件、`global` 影响所有 job 的文件、`jobs.<id>` 显式给出一个 job 的依赖，用 GitHub 的 glob；放在 workflow 文件之外，因为 GitHub 不认识 workflow 里的陌生键）。变更文件不属于任何包或 job 目录、又不在 `ignore`/默认忽略（`**.md`、LICENSE、.gitignore、.editorconfig）里 → 影响所有目录型 job；workflow 文件本身、`.github/actions/**`、配置文件变了 → 全部要跑；变更文件未知 → 全部要跑。**有输出（`outputs:`）或上传 artifact 的 job 永远不略过**（略过会让下游缺东西）。
- ✅ 依赖图级（2026-10-29）：工作区包的发现和依赖——Go（`go.work` 的 `use`，模块之间的 `require` 和指向本地目录的 `replace`）、npm/yarn（`package.json` 的 `workspaces`）、pnpm（`pnpm-workspace.yaml`，支持 `!` 排除）、Cargo（`[workspace] members/exclude`，`path` 依赖，以及与成员同名的依赖——按"同名即本地"保守处理，因为常常靠 `[patch]` 指过去）。job 的范围 = 它工作目录里的包 + 这些包依赖的包（传递）；所以改了底层库，用它的上层 job 会被判为受影响，不相干的包不会。在真实仓库上看过图：vite（287 个 pnpm 工作区包，含 playground 和测试夹具）、tokio（10 个 crate，dev-dependencies 形成环，处理没问题）。
- ⬜ 还没做：①很多项目的 job 不写 `working-directory`，而是用 `cargo test -p x`、`pnpm --filter x`、`npm --workspace`、`go test ./backend/...`、`turbo run`、`nx affected` 这类命令选包——要从 `run:` 里读出这些选择器（按 catalog 描述每个工具的选包参数）才能让 tokio 这类仓库的 job 受益；②矩阵里用 `matrix.xxx` 作为 working-directory 的 job（现在当"整个仓库"）；③内容哈希：Turborepo 是"输入哈希没变就重放上次的结果"，我们现在是"看 diff"。哈希版本不依赖基线提交，并且能重放 outputs 和 artifact（让有输出的 job 也能略过），需要服务端记录每个 job 的"输入哈希 → 上次成功的结果"（C.1）；④go 单模块内的包级依赖（要 `go list`）；⑤`on: workflow_run`/多个 workflow 之间的联动；⑥JS 的 `tsconfig` 路径引用、Python 工作区（uv）。

## B.7 MVP 之后

- ⬜ AI agent：失败 run 的分诊与自动修复（已提过的核心卖点）
- ⬜ 从 Travis / CircleCI / Azure Pipelines 迁移（MVP 只做 GitHub Actions 格式）
- ⬜ 原生配置格式

## B.8 工程与风险

- ✅ 用 Raccoon CI 自己跑自己的 `make check`（dogfood）：`.github/workflows/ci.yml`（checkout、`setup-go` 用 `go-version-file`、gofmt、`make check`、`make build`、`upload-artifact`），在本机 1 分钟左右跑通。这次暴露并修复了一个真问题：缓存/artifact 服务暂存分块默认放在 `/tmp`，本机的 `/tmp` 是 2 GB 的 tmpfs，保存一份 Go 构建缓存（约 750 MB 压缩）时写满，服务端回 500，`actions/cache` 只报 “responded with 500” 和一串 EBADF。现在暂存目录放在缓存/artifact 仓库旁边的持久盘上（`azblob.NewSpoolDir`，每个进程一个私有子目录，超过一天的残留自动清理）
- 🟡 兼容性语料：对 13 个真实开源项目的 90 个 workflow 文件跑了 `plan`，全部解析并展开通过。真正跑起来的有 `spf13/cobra` 的 `test-unix`（8 个 Go 版本 × ubuntu 全部通过，8 个 macOS 实例按设计明确失败）。**还没跑**：commander.js（Node，actions 用 commit SHA 固定）、click（Python + uv + tox）、express、需要 Rust 的项目（没有 Rust 安装器，语料里约 60 个实例需要它）
- ⬜ 已知：同一台机器第二次跑时，`setup-go` 还原缓存会因为 `~/go/pkg/mod` 里已有的只读文件而报 “File exists”（tar 退出码 2，只是警告，job 照常成功）。这是长期使用的自托管机器上 `setup-go` 的已知行为，GitHub 的全新虚拟机不会遇到；对“用户自己的机器”这条路线会常见，需要想办法（例如每次 job 用干净的 HOME，或者还原前清理）
- ✅ 服务端 5xx 的原因以前不会出现在任何日志里：现在 `raccoonci run` 会把 artifact/cache 服务的 5xx 连同原因打到 stderr（`local.LogServerErrors`，只记路径，不记签名）。靠它确认了 cobra 矩阵里 `1.22.x` 的缓存提交失败是 `no space left on device`（`/data` 最低剩 366 MiB）
- ✅ 缓存大小上限和淘汰：`-cache-limit-gb`（默认 10，0 为不限），超过时淘汰最久没被命中的条目；比上限大的条目直接拒绝；提交前检查磁盘剩余空间，不够就先淘汰，淘汰完还不够返回 507。v1、v2 两条协议都覆盖，单测包含淘汰顺序、索引持久化、磁盘不足
- ⬜ 缓存提交时仍要在仓库目录里再写一份完整副本（暂存 + 仓库，瞬时 2 倍）：`Dir` 仓库可以直接把暂存文件改名进仓库，省掉那份拷贝；淘汰只在单进程内有效，多个 `raccoonci` 进程共用同一个缓存目录时各自的上限互相看不见
- ✅ 引擎：不可用 OS 的实例不再占并发槽位排队，立即失败（实测 cobra 的 8 个 macOS 实例一开始就全部报错）
- 🟡 硬编码原则：evolving 的名单都已进 catalog；GitHub 协议本身（字段名、workflow 命令名、上限）和下载地址有意留在代码里，原因见 `CLAUDE.md`

---

# C. 服务端与算力对接

两件事：①**服务端**（长期运行的服务，B 要成为"平台"就必须有；A 路径本身不需要它，GitHub 就是服务端）；②**算力**（把 job 放到机器上跑：InstaBox、用户自己的机器、其他云）。A 和 B 都用算力部分，所以起机、限额、规格、基准里的调度问题放在这里。

## C.1 服务端（第 1 步已做，设计见 `docs/server-design.zh.md`，怎么跑见 `docs/server-dev.zh.md`）

**当前目标（用户 2026-11-01）：先写服务端，写完再按四个仓库拆。** 代码在 `internal/server/…` 和 `cmd/raccoonci-server`，按"以后可以直接搬走"的规则写（只依赖将来进库仓库的包，对外都走自己定义的小接口）。工作顺序见设计文档：1 骨架和入口 ✅ → 2 执行 🟡（本机和 InstaBox 上都跑通了一个 run；matrix、真实 artifact 等没验证） → 3 回报 🟡（commit status、取消、重跑已做；check run 没做） → 4 artifact/cache 落库 🟡（cache 按空间隔离已做并在真机验证；桶和 artifact 元数据落库没做） → 5 密钥与认证 🟡（密钥、变量、角色做了；GitHub 登录和每组织一个桶没做） → 6 定时与 dispatch 🟡（dispatch、定时、起机限额已做，定时没在真 GitHub 上测） → 7 加固。

**关于下面的"未提交"**：这一节里 2026-11-01/02 写的条目标着"未提交"，当时确实没提交；服务端的这些工作现在**都已提交**（`git log` 里 "Server: …" 的提交），标记没有逐条改回来。真正没提交的只是别的会话留在工作区里的改动（`CLAUDE.md`、`cmd/raccoonci/run.go` 等，不属于服务端）。

- ✅ 第 1 步（2026-11-01，**未提交**）：SQLite 存储和编号迁移（`store`）、验签的 webhook（`intake`：HMAC-SHA256 常量时间比较，空密钥一律拒绝，不在允许列表的仓库不入库，投递 id 去重，先存后答 202，后台读 workflow、`trigger.Match`、建 run，重启后补处理）、GitHub 读取和 App 认证（`github`：JWT 换 installation token 并缓存）、只读 API（`api`：`/v1/runs`、`/v1/runs/{id}`、`/v1/events/{id}`，bearer token，没配 token 就 503，`/v1/openapi.yaml`）、密钥读取（`config`：环境变量或 600 权限的文件）、`raccoonci-server replay`。单测都在（含 `-race`）。**真实验证**：把 `cli/cli` 一次真实 push 的签名回放经 Cloudflare Tunnel（`ci.example.com`）发给服务端，用真实 GitHub API 读到 15 个 workflow，建了 3 个 run，和 GitHub 自己在那个提交上跑的三个完全一致；paths 过滤按真实变更文件跳过了一个；重复投递回答 duplicate。**没试过**：GitHub 自己发出的投递（用户要自己建 webhook 或 App，我创建仓库 webhook 被权限拦下，没有绕开）、真实 PR、App 认证对真实 GitHub（只测了假服务）。
- 🟡 第 2 步（2026-11-01，**未提交**）：执行（`internal/server/execute`）。执行器用 `Store.ClaimRun` 原子地领 run，取源码（GitHub 的归档重新打包成 `src/` 一层，`github.Client.SourceTar`）、用 `engine.Run` + `Options.Remote` 把每个 job 实例交给一台机器（`Machine` 接口：`Instabox` 适配 `instabox.Provider`；`Local` 在服务端这台机器上跑 `raccoonci run-instance`，**没有任何隔离**，只用于开发和可信代码，`-local-runner`），机器回传的事件（`internal/wire`，从 `cmd/raccoonci` 里抽出来的 controller↔runner 协议）批量存成这个 run 的日志；服务重启时进行中的 run 记为失败，关机时取消正在跑的；artifact 服务（`internal/server/artifacts`，token 绑定到某个 run 和 job，没有 cache 服务，因为 cache 必须按仓库和分支隔离）；`POST /v1/runs` 手动启动 `workflow_dispatch` 的 workflow（输入按声明检查）；API 加了 `/v1/runs/{id}/jobs` 和 `/v1/runs/{id}/log`（分页，或 SSE，`Last-Event-ID` 可续传）；规格和路由有一个测试保证一致。**真实验证**：`-local-runner` 下，经隧道 `POST /v1/runs` 启动 `bench-lint-docs` 的 workflow，服务端取到真实归档，用真的 `raccoonci run-instance` 跑了 checkout、`setup-node`、三个 `run` 步骤，存了 62 行日志，job 成功，API 读到；一共 4 秒；SSE 经隧道实时推送也通了。**InstaBox 上也跑通了**（runner 快照 `raccoonci-runner-9c482bc`，机器经隧道的 `/agent` 回连）：同一个 workflow 成功，机器 9.8 秒就绪，从请求到结束 16 秒，机器事后已删除（`IBX_API_KEY` 其实在，只是只在登录 shell 里，我的 Bash 工具不是登录 shell，用 `bash -lc` 就有）。**没验证**：matrix、artifact 在真实 job 里、真实 PR、GitHub 自己发出的投递。**已知限制**：（PR 用头提交不是合并提交：2026-11-02 已改，见 C.1 的"PR 合并 ref"一条）；源码里没有 `.git`；没有密钥（`secrets.GITHUB_TOKEN` 为空）；没有 cache 服务；（并发起机的限额已在第 6 步接上）；（`github.run_number` 以前就是 run id：2026-11-02 已改，见 C.1 的"run 编号"一条）；（用 App 认证时手动启动的 run 以前没有 installation id，`Token(0)` 会报错；2026-11-02 已修：见 C.1 的 App 前的补丁一条）
- 🟡 第 3 步（2026-11-01，**未提交**）：回报、取消、重跑。每个 run 在它所跑的提交上设 commit status `raccoonci / <workflow 名>`（开始 `pending`，结束 `success`/`failure`/`error`），用 token 和 App 都行（`-status=false` 关闭）；`POST /v1/runs/{id}/cancel`（排队的立刻结束，运行的取消并删机器）、`POST /v1/runs/{id}/rerun`（同一提交、当时的 workflow 文本、同样的输入，`rerun_of`）。**真机验证**：GitHub 上提交的状态先 pending 后 success；5 秒时取消的 run 为 cancelled、状态 error；重跑成功。**发现并修了一个真泄漏**：创建机器过程中取消，删除被 409 `operation_conflict` 拒绝后就放弃，机器会一直跑到 2 小时期限（`instabox.Client.DeleteMachine` 现在等待并重试，真机验证没有残留；那台漏掉的机器 `rc-r5-1` 我已删）。**check run 的 job 一行（2026-11-02，**未提交**）**：用 GitHub App 时（事件带 installation id），每个 job 实例开始时在后台建一个 check run `raccoonci / <workflow> / <job>`（`in_progress`），结束时更新为 success/failure/cancelled（取消只改 check，库里 job 的结论不变）；建失败只记日志，不影响 job；用令牌（installation 为 0）时不建；`-checks=false` 关闭。单测：客户端 1 个（建、更新、summary 截到 65535、404 的说明）、执行器 2 个（每个 job 一行及结论、无 installation 不建、建失败不让 run 失败）。**真 GitHub 上测过**（2026-11-02，开发 App）：`bench-lint-docs` 的提交上出现 `raccoonci / W7 lint docs baseline / baseline`，由 `example-app[bot]` 建，completed/success，"Success in 18s"。探针 run 失败的几个 job 的 check run 也留在那个提交上（check run 删不掉）。**注解和 job summary（2026-11-02，**未提交**）**：step 的 `::error/warning/notice`（带 file/line/endLine/title）变成 check run 的注解（每个 job 最多 50 条，其余在 summary 里计数；没有可用文件或路径不安全的挂 `.github` 第 1 行），`$GITHUB_STEP_SUMMARY` 成为 check run 的 summary（以前 `wire.Result` 把它丢了：现在 `wire.Result.Summary` 和 `engine.InstanceResult.Summary` 把它带到服务端，runner 已经遮蔽过密钥）。单测：`annotationOf` 表（9 种）、50 条上限、执行器 2 个、客户端 1 个（50 条、标题 255、消息 64 KiB 截断）、wire 往返；真 runner 的测试（`TestRealRunnerAnnotationsAndSummaryReachTheCheckRun`）；真 GitHub（`TestRealCheckRunAnnotations`，GitHub 接受并原样读回，在 `bench-lint-docs` 的提交上留了一个 neutral 的 "raccoonci annotation probe" check run，删不掉）。**没做**：每个 step 的明细、problem matcher 产生的注解、重跑/取消时 check 的 `actions` 按钮；没在真机的真实 job 上看到过注解（那些仓库的 workflow 不会打印）；也也没从 GitHub 发出的投递建出过 run（token 没有 `workflow` 权限，没法加 push 触发的 workflow；需要你 `gh auth refresh -s workflow` 或自己加一个）。**webhook**：已在 `bench-lint-docs` 建了仓库 webhook（id 706032788，指向 `https://ci.example.com/webhooks/github`，push 和 pull_request），ping 回 200，两次 push 和一个 PR 的打开关闭都被收到并记为已忽略；测试分支已删，PR #2 已关（GitHub 不能删 PR）；调试结束要删这个 webhook
- 🟡 第 4 步（2026-11-01，**未提交**）：缓存（`internal/server/caches`）和对象存储。cache 按"空间"隔离：默认分支一个空间（只有默认分支的 run 写），其他每个 ref 各一个空间，能读自己的和默认分支的，不能读别的 ref、不能写默认分支；仓库之间不共用；fork PR 同样走自己的 ref 空间（所以不能给默认分支投毒）；按 token 里的 run 路由到各自的 `cache.Service`，只通告 v1。对象放 `-data-dir` 下，或放用户自己的 S3 兼容桶（`-storage-s3-*`，R2 用 endpoint `https://<账号>.r2.cloudflarestorage.com` 加 region `auto`），启动时检查桶（写、用预签名链接读回、删；桶对匿名请求公开读则警告），桶不能用就拒绝启动。**真机验证**：`bench-go-integration` 的 `setup-go cache: true`，第一次保存 44 MB，重跑恢复，新分支上的 run 通过默认空间恢复，磁盘上只有默认空间；隔离规则的测试在去掉隔离后会失败。**没验证**：桶（S3 路径只用假的测过，没有 R2 的访问密钥——你需要在控制台建一个只管 `raccoonci-dev` 的 API 令牌，key 和 secret 放进 `RACCOONCI_STORAGE_S3_KEY/_SECRET` 的文件）、真实 job 上传 artifact。**没做**：按组织配置的桶（等密钥存储，第 5 步）、artifact 元数据落库和保留期、没人用的空间的清理（GitHub 是 7 天没命中就删）、按仓库的总大小上限
- 🟡 第 5 步（2026-11-01，**未提交**）：密钥、变量和 API token 角色（没有 GitHub 登录）。主密钥 `RACCOONCI_MASTER_KEY` 用 AES-256-GCM 加密密钥后才进数据库（绑定 scope 和名字，轮换用 `RACCOONCI_MASTER_KEY_OLD`），`secrets` 服务按 scope（`org:<名>`、`repo:<owner>/<名>`）保存；run 只拿到 workflow 里按字面写出的 `secrets.NAME`，仓库的盖过组织的；fork 的 PR 拿不到任何密钥（日志里写明），变量都能拿到；密钥只写不读，列表只有名字。API 的路由分 open/read/admin 三档，`read` token 只能读，改动类和密钥、token 管理要 admin（否则 403），token 只存哈希、只在创建时显示一次、可吊销。**验证**：用真正的 `run-instance` 跑（`RACCOONCI_TEST_BIN=dist/raccoonci`）密钥打印成 `***`、日志里任何地方都没有、fork 的是空的；经隧道真实调了存取和 403、吊销。**没做**：每个组织一个桶（存储设置是组织的一条密钥；artifact 要像 cache 按空间那样按组织路由）；用 GitHub 登录（需要 App 的 OAuth；App 已注册，但没配回调地址，也没有前端）；（按仓库限制 token 能看什么：2026-11-02 已做，见 C.1 的"按仓库限制的 token"一条）。发现并修了一个我自己引入的回归：先把"没有 `Bearer ` 前缀的头"也当成 token，测试抓到了
- 🟡 每个组织一个桶（2026-11-01，**未提交**）：`PUT /v1/storage` 给一个组织自己的 S3 兼容桶（R2、S3 都行），保存前先试（写、预签名链接读回、删），不能用就拒绝并说明哪一步失败，对匿名公开读的桶警告；设置和密钥用主密钥加密，放在组织 scope 下名字 `$STORAGE`、kind `storage` 的一行（`$` 不可能出现在密钥名里），workflow 引用不到、密钥 API 列不出也改不了。该组织各仓库的 artifact 和 cache 之后进它的桶（cache 空间按桶版本重建，artifact 按 run 所属的组织路由到各自的服务）；没配的用服务端自己的（`-storage-s3-*` 或本机目录）。**验证**：单测（含路由、版本变化、取不到桶时 500 而不是悄悄落到本机）；经隧道真实调了一次，指向不存在的主机的桶被拒绝、没有保存、密钥没有出现在返回里。**没验证**：真实的桶（R2 的访问密钥还没给）。
- 🟡 第 6 步（2026-11-02，**未提交**）：定时触发和起机限额。①**定时**（`internal/server/schedule`，cron 解析器从 `ghrunner` 搬到 `internal/cron`，`ghrunner` 留一层别名，将来进库）：每 15 秒看一次；看收到过事件的仓库和 `-allow` 里写明全名的仓库，只认**默认分支**上的 workflow（缓存 5 分钟），到点时在分支最新提交上重新读文件、cron 条目必须还在才触发；一次触发记成事件（id = 仓库+文件+条目+分钟），重启不会重复；停机时错过的不补，卡顿后只出一个 run；`github.event_name` 是 `schedule`，`event.schedule` 是那条 cron。`-schedule=false` 关掉。单测 9 个（到点、两条 cron、卡顿、重启不重复、条目被删、未允许的仓库、`Extra`、读取失败后保留旧信息、默认分支名）。**真机没测**：找遍 Raccoon-CI 的 7 个基准仓库，没有一个带 `schedule:`，令牌又没有 `workflow` 权限没法加，所以真实 GitHub 上的触发只有单测。没做：GitHub 对 60 天无活动仓库停定时任务的规则；通配 `owner/*` 里还没收到过事件的仓库发现不了。②**起机限额**（`instabox.StartLimit`，`Provider.Limit`，服务端 `-starts-per-minute`，默认 10）：62 秒滑动窗口数自己的起机，超了排队等；429 之后所有起机停 20 秒再问（最多约 7 分钟，防止别的原因的 429 永远转圈）；日志 `waited … for the machine start limit`。单测 6 个（窗口、429 后暂停、无限制、context 结束、429 后重试、没有 Limit 时 429 照样报错）。**真机（2026-11-02）**：限额设成 2，连发 4 个 run：前两个 9–11 s 起机，后两个分别等了 1 m 0.5 s 和 1 m 1.5 s 才起，4 个都成功，机器都已删。**没测**：真的收到 429（只有假 API 的测试）；别的 InstaBox 用户占用同一组织名额的情形。**顺带发现**：InstaBox 账号里所有快照都不见了（`GET /v1/snapshots` 为空），旧 runner 快照 `…usnou` 不存在，run 全部 "snapshot not found"；重建为 `raccoonci-runner-450db2e-wt`（`sn_EXAMPLE_SNAPSHOT_ID`，二进制含工作区里别的会话未提交的改动），服务端现在用它
- 🟡 第 7 步·重启后恢复（2026-11-02，**未提交**）：以前重启会把进行中的 run 记为失败。现在：`runs.attempts`（迁移 0005）记执行器取走次数，`Store.RequeueInterrupted`（启动时）和 `Store.Requeue`（优雅停机时，执行器发现是服务在停、不是有人取消）把 run 放回队列并删掉本次尝试的 job 行（日志保留，新增一条警告 "attempt N"）；取走 3 次（`Executor.MaxAttempts`）后记为失败，防止一个能弄垮服务的 run 无限循环；停机重排时不再向 GitHub 报 "Cancelled"，状态保持 `pending`。从头重跑，不是中途接着跑。5 个单测（尝试计数与上限、启动重排与终止、停机后重排再被下一次启动跑完、用尽次数记失败）。**真机（`bench-lint-docs`）**：run 中途服务被杀 → 下次启动重排，第 2 次成功；run 中途 SIGTERM → 约 15 s 后（先删机器）日志 `queued again`，重启后第 2 次成功。没试：长 run 的后段重启；崩溃后旧机器要等 2 小时期限（不知道机器 id，没存）
- 🟡 第 7 步·artifact 元数据落库与保留期（2026-11-02，**未提交**）：`internal/artifact` 加了可选的 `Index`（`Load`/`Save`/`Delete`）、`Keep`/`MaxKeep`，以及 `Artifacts`、`DownloadURL`、`Expire`；服务端 `artifacts` 表（迁移 0006，每个 artifact 服务一个 `scope`：`disk` 或 `<org>@<桶版本>`），完成的 artifact 在 Finalize 时先写库再算完成（写库失败就不算完成，删除时先删记录再删数据）；重启后按 scope 载入，编号接着往下排，已用过的名字仍算用过。新 API：`GET /v1/runs/{id}/artifacts`、`GET /v1/runs/{id}/artifacts/{artifact}/download`（302 到 1 小时有效的预签名链接，`read` 角色）。保留期：默认 90 天，`-artifact-retention-days` 同时是默认值和上限，步骤的 `retention-days` 只能更短；每小时和启动时清理一次（经各 run 的服务删数据+记录；换过桶的旧记录只忘记录，旧桶里的对象留着；服务取不到的下次再清）。单测：库层 6 个（重启后保留、名字和编号延续、删除先删记录且失败时不丢、Finalize 写库失败不算完成、过期、上限）、服务层 3 个（真实上传协议+SQLite 重启后列出并下载、清理、孤儿记录与取不到的记录）、API 2 个、store 1 个。**真机**：现有数据库上迁移成功，三个路由的 200/404/404 正常；**没有真的在服务端上跑过上传 artifact 的 job**（基准 workflow 没有一个用 `upload-artifact`，令牌又不能加 workflow）。没做：没上传完的分片清理（桶的生命周期规则）、artifact v3、整体 sha256 校验
- 🟡 第 7 步·缓存清理（2026-11-02，**未提交**）：①7 天没人恢复过的条目删除（`-cache-max-age-days`，和 GitHub 一样）；②变空且没有上传的空间连目录一起删；③一个仓库所有空间加起来的上限（`-cache-repo-limit-gb`，默认 10，0 不限）：按仓库跨空间删最久没用的条目；两者由 `Sweep` 做，启动时和每 15 分钟一次，所以仓库上限是"软"的（每个空间自己的 5 GB 上限仍是每次上传时检查）。清理先读 `index.json` 判断，不为不需要动的空间打开服务。空间所属组织写进 `<空间>/org`（目录名只是哈希），旧空间没有这个文件时：有 `objects/` 就当本机磁盘，否则不动。`cache.Service` 新增 `Entries`/`ReadIndex`/`Evict`/`Expire`/`Idle`；顺带修了：恢复（命中）的时间以前要等下一次提交才落盘，重启后常用条目可能显得"7 天没人用"，现在最多每 5 分钟写一次并在关闭时写。单测：库 5 个、服务 7 个（过期、命中续命、空空间删除后能重建、有上传的不删、仓库上限跨空间、org 文件、未知桶的空间不动），仓库上限的测试做过变异检查。**真机**：重启后 `bench-go-integration` 在 InstaBox 上真的恢复了 44 MB 缓存（"Cache restored successfully"），`used` 立刻写盘，`org` 文件出现，清理没动这个活空间。**过期和仓库上限本身没有在真机上看到**（最短是 1 天，条目才十几个小时，也没法拨动真机的时钟）。没做：桶里被中断的分段上传的残片
- 🟡 第 7 步·日志保留期（2026-11-02，**未提交**）：已结束 run 的日志行默认保留 90 天（`-log-retention-days`，可写小数，0 永久），`internal/server/retention` 在启动时和每小时清理（每批 100 个 run、一批一个事务）；`runs.log_purged`（迁移 0007）标记，日志路由（分页和 SSE）对这样的 run 回 410，run 的 JSON 里有 `log_purged`，run、job、结论都保留。单测：store 1 个、retention 1 个（250 个 run 跨三批，只清过期的）、API 1 个。**真机**：开发库上用 `0.01` 天，早于 14 分钟的 20 个 run 清掉 766 行，410 和 `log_purged:true` 正常，12 分钟前的 run 日志还在；已改回 90 天。没做：旧 webhook 投递的原始载荷和旧 run 的 workflow 原文不清（重跑要读）；SQLite 文件不缩小
- 🟡 数据库：事件、run、job、日志、artifact 元数据、密钥/变量/每组织桶设置、API token、installation、runner-images 快照、OIDC 签名密钥都已入库（迁移 0001–0010）；**还没有**：catalog 数据（见下面"catalog 待定"）、服务端自己的默认桶设置（现在是启动参数加环境变量，见 C.1 凭证一条）
- ✅ installation 入库（2026-11-02，**未提交**）：迁移 0010（`installations`、`installation_repos`）；`installation` 和 `installation_repositories` 投递（GitHub 对每个 App 自动发，不要求仓库在 `-allow`）只记录不跑（created/new_permissions_accepted/deleted/suspend/unsuspend、仓库增删）；`instsync` 启动时和每小时从 GitHub 读（`App.Installations`、`Client.InstallationRepos`），GitHub 不再列出的就忘掉，读不到时不删任何东西、读不到仓库的保留旧列表；`installationFor` 先查这张表再查事件再问 GitHub；`GET /v1/installations`（限定仓库的 token 403）。单测：store 1 个（selected/all、增删、挂起、删除级联、替换）、intake 3 个、instsync 3 个、github 客户端 2 个（含翻页）、API 1 个。**真机**：启动时读到真实的安装 INSTALLATION_ID_EXAMPLE（8 个仓库）。**没做/没见过**：GitHub 自己发的这两种事件还没见过（装/卸 App 才会产生）；重新投递旧的 `installation` 事件会把仓库列表退回当时的样子，直到下一个事件或每小时的读取
- ✅ API（2026-11-02 重写，原写"缺重跑、取消、密钥、认证登录"已过时）：run、job、日志（含 SSE）、事件、artifact 及下载、手动启动、重跑、取消、密钥/变量、每组织桶设置、token 及按仓库限制、installation 列表、GitHub 登录（见下一条）；规格在 `openapi.yaml`，一个测试保证路由和规格一致。UI（frontend）可以开始了
- 🟡 用 GitHub 登录（2026-11-02，**未提交**，`internal/server/auth`）：OAuth 授权码流程，用 App 自己的用户授权。login 把浏览器送到 GitHub（state 带 HMAC 签名、绑到浏览器的 cookie：HttpOnly/Secure/SameSite=Lax，防"登录 CSRF"）；callback 用 code 换用户 token，问出登录名和他能访问的、App 所装的仓库，**立即吊销用户的 GitHub token**，发一个**限定到这些仓库、12 小时过期的 read 会话**（存为 `api_tokens` 的一行，迁移 0011 加了 `user`、`expires_at`；`LookupToken` 认过期）；只有 `-auth-admins` 里的登录名拿 admin；一个仓库都访问不到的 403 且不发会话；`redirect` 必须匹配 `-auth-redirect` 且会话放在 URL 片段里不放查询串；`GET /v1/auth/me`、`POST /v1/auth/logout`（被限定的 token 也能用）；过期超过一天的会话由 retention 清掉。**设计取舍**：会话是限定仓库的 token，复用已有的范围机制，而不是另造一套权限；创建时就定下能看什么（之后被授权的仓库要重新登录）；不做刷新令牌（到期重新登录）。**测试抓到一个真 bug**：吊销用户 token 的 DELETE 没带 JSON 体，GitHub 会忽略它，"尽力吊销"其实从来没发出过有效请求——已修并有断言。单测：auth 包 8 个（跳转与 cookie、完整流程含两个安装的去重小写仓库、过期、管理员、无仓库者、没有 cookie/别人的 cookie/没签名的 state/过期 state、code 只能用一次、redirect 白名单与片段、配置检查）、store 2 个、API 3 个、retention 1 个。**真机**：服务端带 client id 和密钥文件启动，经隧道的 login 302 到 GitHub（client_id、回调地址、签名 state、cookie 都对），恶意 redirect 和没有 cookie 的 callback 都是 400。**没做/待你做**：**还没有人在浏览器里真走完一遍**——要先在 App 设置里登记回调地址 `https://ci.example.com/v1/auth/github/callback`（我看不到 App 设置，GitHub 对未登录的请求只给登录页，所以我无法确认它登记了没有）；会话撤销（管理员强制某用户下线）没做，只有用户自己 logout 或到期；没有前端，所以 `-auth-redirect` 还没有真实用途
- ✅ 把状态回写 GitHub（第 3 步）：commit status、job 级 check run、注解、job summary 都做了（见第 3 步一条）；每个 step 一行和 problem matcher 的注解没做
- ✅ secrets / vars 的加密存储（第 5 步）
- 🟡 凭证和机密放数据库（服务端版本；本条 2026-11-02 重写，原来整条是 ⬜）。**已做**：①信封加密入库——密钥/变量、每组织的桶设置和密钥、服务端自己的默认桶设置（`PUT /v1/storage` 的 `org:"*"`，**2026-11-02，未提交**：先试再封存、立即生效、优先于 `-storage-s3-*` 启动参数、删掉后退回启动参数/磁盘）、OIDC 签名密钥，都用主密钥 AES-256-GCM 封存，绑定 scope 和名字，读出后只在内存用，日志和 API 响应里没有（测过）；②按组织隔离（不同组织不同桶；cache 按空间、artifact 按组织路由）；③机器永远拿不到桶密钥，只拿预签名链接；④workflow 机密只在派发任务时解密，只给 workflow 里按字面写出的，fork 的 PR 一个都不给；⑤job 的 `GITHUB_TOKEN` 是每个 job 一个、范围最小、结束即吊销的 installation token。**还留在环境变量/文件里的**（有意的，鸡生蛋问题）：主密钥 `RACCOONCI_MASTER_KEY`、webhook 密钥、App 私钥、InstaBox 密钥、bootstrap API token——它们是打开数据库里其余东西的钥匙，放进库里没有意义；要换成 KMS/密钥管理服务是以后的事。**没做/待定**：能用短期凭证（STS）就不存长期桶密钥（R2/S3 的 STS 各不相同，需要有真实的云账号来设计）；桶密钥轮换的流程（现在是重新 `PUT` 一次）；workflow 机密"InstaBox Org Secret 占位符代理换头"，让机器上根本没有真值（InstaBox 一侧的机制，要和它一起设计，机密现在随任务明文发到机器）；`raccoonci run` 的 `-secret` 是本地环境变量，这是本地引擎的用法，不属于服务端
- ✅ 切到 GitHub App（2026-11-02）：开发 App「Raccoon CI (example App)」（App ID APP_ID_EXAMPLE，slug `example-app`，装在 Raccoon-CI 组织的全部 8 个仓库，installation INSTALLATION_ID_EXAMPLE；权限 contents/checks/statuses/pull_requests/issues/deployments 读写、actions 只读，订阅 push 和 pull_request；私钥 `~/.config/raccoonci/app.pem`，webhook 指向 `https://ci.example.com/webhooks/github`）。服务端用 `RACCOONCI_GITHUB_APP_ID`+`_KEY_FILE` 启动（`github_auth=app`），`bench-lint-docs` 上旧的仓库 webhook 706032788 已按文档第 5 步删除（避免同一事件两个 run）。**真机验证**：`POST /v1/runs` 用 App 找到 installation、读 workflow 和提交；在 InstaBox 机器上跑成功（机器 9.8 秒就绪，共 19 秒）；提交上有 App 建的 status（pending→success）和 check run；GitHub 自己发的 push 带签名和 installation id 经隧道到达并被记为忽略；runner-images 轮询在真网络上第一次跑通。文档 `docs/github-app.md` 的权限表按实际注册的权限更新（说明 App 权限是 job 的 `GITHUB_TOKEN` 的上限）。**没做**：真 PR 的投递（合并 ref、fork）；由 GitHub 投递建出的 run（这些仓库没有监听 push 的 workflow）；服务现在是我在后台启的（`nohup`，没有 systemd，沙箱重启后要手动起；启动脚本在会话临时目录里，没入库）；调试结束要删隧道和 DNS（见记忆笔记）
- ✅ 注册 GitHub App 前的补丁（2026-11-02，**未提交**）：用 App 时，手动启动（`POST /v1/runs`）和重跑的 run 没有 installation id，取源码和报状态都会失败。现在 `intake.Handler.Dispatch/Rerun` 先在已存的事件里找这个仓库最近的 installation id，找不到就用 App 的 JWT 问 GitHub（`github.App.InstallationOf`，`GET /repos/{repo}/installation`）；App 没装在这个仓库上回 404 "the GitHub App is not installed on …"；事件里记下这个 id，执行器因此读得到。用令牌时不需要，仍是 0。单测：intake 2 个、github 1 个（含"500 不会被当成没装"）。**没在真 GitHub 上测**（App 还没注册）
- 🟡 工作目录清理（2026-11-02，**未提交**）：服务启动时（在把中断的 run 重新排队之后）清空执行器的 `work/`、本机 runner 的 `jobs/`，以及 artifact 和 cache 两个上传暂存目录（`artifacts/spool`、`caches/cache-spool`）：崩溃时没来得及删的残留。前提是一个数据目录只有一个服务在用（第一版就是单实例）。正常结束的 run 本来就会删自己的目录。单测 4 个（执行器+本机 runner、两个暂存目录）。没有在真机上看到崩溃后清理（只有单测）
- ✅ 仓库之间公平领 run（2026-11-02，**未提交**）：`ClaimRun` 在排队的 run 里优先挑"所属仓库（不分大小写）正在跑的 run 最少"的，一样多先到先得；排了很多 run 的仓库饿不死别的，只有一个仓库有活时仍然独占全部名额，不需要配置。**取舍**：用公平选取而不是硬上限（硬上限要配数字，还会在别人没活时白白空着名额）；等待并发组的 run 也算"在跑"（它们是 in_progress），所以一个仓库的长队列会让它的新 run 稍后于别的仓库，可以接受。单测 2 个（跨仓库交错且同仓库 FIFO、不分大小写、单仓库独占按序）。（按仓库/组织的月额度：2026-11-02 已做，见下一条"用量与额度"）
- ✅ 用量与额度（2026-11-02，授权后按稳妥方案做）：①**记账**——每个 run 结束时把它的 job 实例占着机器的时间（从向 provider 要机器到实例结束，含起机；run 内合计后向上取整到秒）按 UTC 日期和仓库加进新表 `usage`（迁移 0012，`Store.AddUsage/UsedSince/ListUsage`）；②**额度**——`-quota-repo-minutes` 和 `-quota-owner-minutes`（默认 0 = 不限），按 UTC 自然月；用完了的仓库/所有者的新 run 在**领到之后、取源码和起机器之前**就失败，日志里写明"已用 N 分钟/共 M 分钟，几号恢复"，状态回报给 GitHub 为失败；③`GET /v1/usage?month=YYYY-MM`（每仓库月合计 + 每天明细 + 当前额度；限定仓库的 token 只看到自己的）。**取舍**：a) 数的是已结束的 run，所以同时在跑的会让额度略微被超过——精确到"在跑的也算"要预估时长，得不偿失；b) 额度用完时让新 run **失败并说明**，而不是留在队列里不动（排队会让人以为服务坏了，也会在月初一次涌出来）；c) 记的是秒不是钱：价格在 InstaBox 一侧会变，换算留给读用量的人；d) 默认关，因为替用户定一个数字没有稳妥的值。单测：store 1 个（按日加总、大小写、仓库/所有者范围、`_` 不是通配符、上月不算）、执行器 4 个（记账、仓库额度含另一个仓库不受影响且没用机器、所有者合计、下月重新开始和不设额度）、API 1 个。**没做**：各仓库不同的额度、快用完的提醒、重跑是否该免额度（现在照常计）、**真机**（开发服务，InstaBox）：不设额度时 3 个并发的真 run 共记了 102 机器秒（每个约 34 秒，含起机）；把 `-quota-repo-minutes` 设成 1 后重启，下一个真 run **立刻**失败，信息是"raccoon-ci/bench-lint-docs has used its machine-time allowance for this month (1 of 1 minutes); runs start again on 2026-12-01 UTC"，没有起任何机器；随后恢复不限额，再跑一个成功，提交上的状态改回 success。注意它给那个开发提交留下过一次失败状态（现已被覆盖）
- 🟡 磁盘保护（2026-11-02，**未提交**）：①数据目录所在的盘剩余空间低于 `-min-free-mb`（默认 1024，0 不检查）时，执行器不再领新的 run，留在队列里，日志警告一次，空间回来后自动继续；②一个 run 的源码包超过 `-max-source-mb`（默认 2048）就让这个 run 失败，日志写明超限，工作目录不留残渣（`cappedWriter` 在写入时截断，不是下载完再量）。单测 2 个（盘满时 run 保持排队、空间回来后跑完；超大源码失败且无残留）。③artifact 服务同样用 `-min-free-mb`：盘不够时 `CreateArtifact` 回 429（`resource_exhausted`，消息写明磁盘快满），空间回来后恢复（`artifact.Config.Free/MinFree`；单测 1 个）。**没做**：真正的配额（按仓库或组织限制总占用）；已经开始的上传不会被中途拒绝（只在开始新 artifact 时检查）；没有在真机上把盘写满看过（只有注入的 `Free`）；不确定 `upload-artifact` 对 429 的重试行为（可能会退避重试几次再失败，没试）
- 🟡 服务端定期轮询 runner-images（2026-11-02，**未提交**）：`internal/server/imagesync` 启动时和每 `-runner-images-interval`（默认 6 小时，0 不轮询）按 catalog 里的镜像读 `runnerimages.Fetcher`（徽章，不需要令牌），存进新表 `documents`（迁移 0008，`Store.PutDocument/GetDocument`，整份替换）；`Syncer.Snapshot` 是内置快照盖上轮询到的数据（从不比内置的少；读库失败时仍给内置的并记警告），执行器每个 run 用它（`Executor.Images`）。读不到的镜像保留上次的好数据；别名指向没跟踪的镜像时拒绝；存的文档损坏时被下次轮询覆盖。单测：syncer 4 个、store 1 个、执行器 1 个。**（2026-11-02 补）数据随任务发给机器**：run 规划用的那份快照（约 10 KB）放进 `wire.InstanceSpec.Images`，`run-instance` 用它代替二进制里内置的（`runnerimages.Snapshot.JSON/ParseSnapshot`，拒绝不可能是真的快照：没有镜像、镜像名对不上、超过 1 MiB 或 64 个镜像，悬空别名被丢掉）；数据不可用时只在 job 日志留一行并用内置的；旧机器忽略这个字段所以向后兼容。单测：往返与拒绝各 2 个、执行器 1 个（每个 job 都带规划时的版本）、真 runner 2 种情形。**限制**：没用 ETag（徽章和报告的请求量很小）；没在真网络上跑过（只有假数据源，命令行的 `runner-images update` 走的是同一个 `Fetcher`）
- 🟡 PR 合并 ref（2026-11-02，**未提交**）：`pull_request` 的 run 取 `refs/pull/N/merge` 的源码（和 GitHub 自己的 runner 一样是头合进基分支的结果），ref 刚开 PR 时还没有，最多问 4 次、隔 2 秒（`Executor.MergeTries/MergeWait`，每次先清空文件，失败的一次不会留残渣）；一直没有（冲突）就跑头提交并在 run 日志里写警告。`SourceTar` 现在按段转义 ref，斜杠保留。单测：执行器 3 个（先 404 后成功且无残渣、一直没有则退回头并警告并问了 4 次、push 不用合并 ref）、客户端 1 个。**没在真 GitHub 上测**（没有真 PR 的投递）；`github.sha` 仍是头提交的 id（GitHub 上是合并提交的 id，要知道它得多一次 API 调用，没做）；`pull_request_target` 没动；PR 在跑的时候基分支又前进了，不会重新合并
- ✅ run 编号（2026-11-02，**未提交**）：`github.run_number` 按仓库+workflow 文件数事件启动的 run（重跑不算新编号），`github.run_attempt` 数同一编号下的重跑（`Store.RunNumbers`，执行器每次 prepare 时算，两条 COUNT 查询）。单测：store 1 个（两个 workflow 交错、两次重跑）、`githubContext` 的断言补了编号和尝试。注意 `run_attempt` 数的是用户的重跑，不是服务重启后的重新排队（那个是 `runs.attempts`）。没做：`github.run_id` 在重跑时仍是新的 id（GitHub 上不变），改它要动 artifact/cache 的 token 范围
- ✅ 按仓库限制的 token（2026-11-02，**未提交**）：`POST /v1/tokens` 的 `read` token 可以带 `repos`（`owner/name` 或 `owner/*`，不分大小写，最多 100 个；迁移 0009 `api_tokens.repos`）。这样的 token 在 `GET /v1/runs`（`RunFilter.Allow`，SQL 里 LIKE 转义了 `_` 和 `%`）里只看到这些仓库的 run，别的仓库的 run/事件（含 jobs、日志、SSE、artifact 及下载）回 404 且正文与不存在的一样（不泄露存在）；其余任何路由都是 403，**新路由默认对限定 token 关闭**，要放开必须显式加进 `inScope`；`admin` 不能限定（400，因为改东西没法按仓库诚实地限制）；列表里显示 `repos`。**取舍**：限定用路径模式在 `auth` 中间件里统一做，而不是每个处理函数各自检查（漏一个就是泄露）。单测：store 4 个（模式规范化、匹配、`Allow` 过滤含下划线不是通配符、token 保存 repos）、API 3 个（各读路由、owner 模式、admin 不可限定与非法模式）；变异检查：`inScope` 放行一切时 8 处断言失败。**真机**（开发库，迁移 0009 在现有库上成功）：限定到别的仓库的 token 列表为空、run 4 回 404、`/v1/tokens` 回 403；限定到 `raccoon-ci/*` 的看到 4 个 run、run 4 回 200；两个测试 token 随后已吊销。**没做**：限定 `admin`；按组织而不是按仓库列表的 token；artifact 下载链接（已签名的预签名 URL）一旦发出就不再受限（1 小时有效）
- ✅ 来自 fork PR 的 workflow 的 secrets 策略：fork 的 PR 一个密钥也拿不到、日志里写明，变量照给（第 5 步，真正的 `run-instance` 测过）；`pull_request_target` 的策略没单独做
- ✅ 就绪检查 `GET /readyz`（2026-11-02，`api/ready.go`，`Store.Ping`，`-check URL`）：`/healthz` 只说进程在，`/readyz` 说能不能干活——数据库能拿到写锁（`BEGIN IMMEDIATE` 后回滚，不写任何东西，被锁死/只读/丢失都会失败，3 秒内放弃）、数据盘剩余大于 `-min-free-mb`；不要 token，失败回 503 并只写哪项检查失败，原因（含路径、数字）只进日志。`raccoonci-server -check URL` 供 Docker `HEALTHCHECK` 用（distroless 没有 curl）。单测：存储层用另一个事务持锁验证 Ping 失败又恢复且没写行；API 层验证每项状态、不泄露原因、没有检查时只说进程在；`-check` 跟随状态码。已写进部署文档（中英）。
- ✅ `GET /metrics`（2026-11-02，`api/metrics.go`，`Store.RunStats`）：Prometheus 文本格式，admin 的 bearer token。`raccoonci_check{check}`（和 `/readyz` 同样的检查，1/0，原因不进输出只进日志）、`raccoonci_runs{status,conclusion}`（数据库里各状态的 run 数）、`raccoonci_oldest_queued_run_age_seconds`（最老的排队 run 等了多久，没有为 0：run 没人取时告警看它）、`raccoonci_build_info{version}`。标签值按格式转义。单测：没 token 401，检查失败为 0 且原因不外泄；存储层按状态和结论计数、找最老排队。**没做**：执行器内部的指标（正在跑的机器数、起机等待、各 job 耗时）——需要执行器暴露数字，等有真实需求再加；没用真的 Prometheus 抓过，只对照了文本格式写。
- 🟡 部署文件和文档（2026-11-02，`deploy/`、`docs/deploy.md`/`.zh.md`）：①`deploy/Dockerfile`（多阶段，静态二进制，distroless、非 root 用户 65532，`/data` 卷）；②`deploy/raccoonci-server.service`（systemd，`DynamicUser`，密钥用 `LoadCredential` + `%d` + `NAME_FILE`，一整套限制）；③`deploy/Caddyfile`（HTTPS、大请求体、不缓冲 SSE）；④文档：需要什么、密钥怎么生成、两种部署方式、反向代理、Cloudflare 的 100 MB 限制、第一次启动检查、升级、**备份与恢复**（`VACUUM INTO`）。**实测了**：运行镜像（用宿主机编的二进制）启动、写库、`/healthz` 和 API、重启后数据还在、拒绝别人可读的密钥文件；**读不了属主为 root 的私钥**（文档里原来的示例会 `permission denied`，实测发现后改了）而属主 65532 模式 400 的可以；单元文件通过 `systemd-analyze verify`，暴露度 1.2 OK；真二进制在同样的限制（seccomp 过滤、禁止可写可执行内存、地址族、不能提权）下能跑，`LoadCredential` + `%d` + `$RACCOONCI_ARGS` 拆词在真的用户单元文件里正常；`caddy validate` 通过；备份和恢复在开发库的拷贝上演练过。**2026-11-02 用户允许后在沙箱里临时装成系统服务实测（systemd 257，装完已全部删除，`/etc/raccoonci`、`/var/lib/raccoonci-server`、单元和二进制都不在了）**：`DynamicUser`（uid 63382，`NoNewPrivs`、seccomp 生效）、`StateDirectory`（库和 `-wal`/`-shm` 都是 600）、`LoadCredential` + `%d` + `NAME_FILE`、`EnvironmentFile` 里的 `RACCOONCI_ARGS`（后面的 `-listen` 覆盖前面的）、`/readyz` 200 与（`-min-free-mb` 设很大时）503、`-check` 退出码都正常；用的是临时生成的假密钥和 `-allow test/test`。**实测中发现并修了一个会让这个单元根本起不来的问题**：systemd 257 把凭证交成 root:root、模式 440、用 ACL 放行服务用户的文件（目录 550），服务端的"别人可读"检查把它当成别人可读而拒绝启动（`webhook-secret is readable by others (mode 440)`）；之前用用户单元文件测 `LoadCredential` 没碰到，因为那边不是这样交的。现在 `config.OthersCanRead`/`secret` 只在文件直接位于 `$CREDENTIALS_DIRECTORY`、文件和目录都属 root:root、且没有 other 位时放行组位（App 私钥的检查同样走这个函数）；单测：同样的 440 在别处、不属 root、目录对所有人开放、变量未设、有 other 位都仍被拒绝。**没实测**：①`docker build` 的构建阶段（沙箱的 TLS 代理让容器里 `go mod download` 报证书错误，与文件无关；构建命令在宿主机上跑过）；②~~需要系统管理器的指令~~（已实测，见上；`ProtectSystem`/`ProtectHome`/`CapabilityBoundingSet` 没单独验证每一项，只验证了整套下服务能启动、写状态目录）；③nginx 的写法；④绑定挂载的 65532 文件；⑤资源需求（CPU/内存/磁盘）没量过。**实测中发现并修了一个安全问题**：开发库的 `server.db` 及 `-wal`/`-shm` 是 `644`（SQLite 默认），同机其他用户能读 token 哈希、webhook 原始载荷、日志。现在服务端每次启动先用 `600` 建/收紧数据库文件，数据目录能被其他用户进入时警告（不替操作员改目录）；单测 3 个，变异检查（让函数什么都不做）会失败；开发服务上实测旧库被收紧。
- ⏸ catalog feed（**待定，等你**；用户 2026-11-02 说默认地址未发布，不要编造）。缺三样，都不是我能替你定的：①feed 的默认地址（没有就不设默认，`-catalog-url` 显式给才拉，现在命令行的 `raccoonci` 就是这样）；②签名生成流程和工具（ed25519 密钥谁保管、怎么发布）；③服务端怎么用它——设计上 catalog 只"命名"东西、不含命令和 URL，所以可以安全地入库并随任务发给机器（和 runner-images 数据一样），但服务端现在只用内置的 `catalog.Default()`，没有拉 feed 的 `-catalog-url`。建议的做法（等你同意再做）：服务端加 `-catalog-url`/`-catalog-key`，拉到的文件存 `documents` 表，执行器用"内置+feed"规划，并把有效的 catalog 随任务发给机器（`InstanceSpec` 加一个可选字段，机器上校验后叠加）
- 依赖这里的 B 项：OIDC 和真实 `GITHUB_TOKEN`（B.3 未决项）、artifact/cache 的持久化与按分支隔离（B.3 搁置项）、内容哈希的按需执行（B.6 ③）、完整的 `github` 上下文（B.5）

## C.2 算力提供方（provider）

优先级：用户自己的机器 → InstaBox → Boat.dev → DigitalOcean/Linode。

- 🟡 机器上的 agent 和主控协议（`internal/dispatch`、`raccoonci agent`）：机器主动向主控发起 HTTPS 连接，领取任务、拉源码包、回传日志和心跳、报告退出码；不走 provider 的 exec（InstaBox 全账号 32 并发的限额），机器不需要公网地址。每个任务一个随机令牌（主控只存它的哈希，令牌只开自己的那个任务），日志按偏移量上传所以重试不重复、有缺口会拒绝；机器第一次联系超时（2 分钟）或之后 30 秒无声都会让 `Run.Wait` 报错（机器死了能发现）；agent 只接受 https（或回环地址的 http），因为令牌和机密都走这条连接；配置在 `/etc/raccoonci-agent.json`（令牌不放命令行）。用真实二进制测过：源码包过去、`actions/checkout` + `run` 步骤成功、失败退出码回传、机密在日志里被遮蔽。**还没做**：主控的对外进程和域名（开发期可用 `instabox-port` 发布，地址公开所以每个请求都鉴权，且只在这台机器运行时有效）；产物（artifact）和缓存在机器上跑完就丢，没有回传；机密现在随任务明文发到机器（要换成 Org Secret 占位符，或至少限定为机器专属令牌）；一次只一个任务，机器用完即删
- ⬜ 用户自己的机器（agent 连接）：协议已有，缺注册和长驻模式
- 🟡 InstaBox（Debian 13）：代码已写（`internal/instabox`：API 客户端 + `Provider`；`raccoonci run -on instabox -listen ADDR -controller-url https://… -snapshot ID`，密钥读 `INSTABOX_API_KEY`），一台机器跑整个 workflow。流程：从快照建机器（带 `deleteAfterSeconds` 兜底，主控进程死了服务端也会删）→ 等操作完成 → 文件接口写 `/data/home/.raccoonci-agent.json`（令牌）→ 快照里的 systemd 服务 `raccoonci-agent` 读到后连主控 → 跑完删机器（取消也会删）。API 规格来自 `https://api.instabox.sh/v1/openapi.json`（公开，能查全部接口）。新快照 `raccoonci-runner-agent-23db1cb`（`sn_EXAMPLE_SNAPSHOT_ID`）带 agent 服务；机器内 home 是 `/data/home`，不是 `/home/user`。**真实端到端已跑通（2026-10-29）**：本项目自己的 `ci.yml`（checkout、setup-go、gofmt、`make check`、build、upload-artifact）用 `-on instabox` 在新建机器上成功，job 用时 1m15s，整个命令 1m35s（含建机器、传源码、删机器），机器事后已不在列表里；主控通过 `instabox-port` 发布的地址被机器连上。API 要求所有写操作带 `Idempotency-Key`（客户端每次随机生成）。密钥读 `INSTABOX_API_KEY`，没有则读 `IBX_API_KEY`（`INSTABOX_API_KEY` 是机器上的保留名，org Secret 只能用别的名字；shell 里是占位符，代理换头）。**现在是一个 job 实例一台机器（2026-10-29）**：引擎在主控这边解析 needs 和矩阵（`engine.Options.Remote` 收到每个已解析的实例），每个实例发给一台新机器，机器上 `raccoonci run-instance` 用 `engine.RunInstance` 跑（装环境、选镜像、runner 都在机器上），事件按 JSON 行流回来、结果带 outputs 回来（`wireReader`，`value.FromJSON` 保持键顺序），所以 `needs.*.outputs`、动态矩阵、fail-fast、`max-parallel` 都照常。artifact 和 cache 服务由主控用公开地址提供（`publicServe`，链接和环境变量都用 `-controller-url`），机器直接访问，所以 job 之间的产物/缓存通了。实测 cobra 全部 workflow：10 台机器并行（lic-headers、golangci-lint、8 个 ubuntu 矩阵），整条命令 56 s，ubuntu 实例各 9–18 s，8 个 macOS 和 1 个 Windows 实例按设计在主控这边直接失败，没有占机器；10 台并发创建时机器就绪 6–22 s（单台约 6 s），事后全部删除。runner 快照现在是 `raccoonci-runner-ri-4008bd4`（`sn_EXAMPLE_SNAPSHOT_ID`），旧的两个已用 API 删掉。`-parallel` 在远程模式默认 8。**收尾（2026-10-29）**：源码包不再带 git 忽略的文件（`.git` 保留；不是 git 仓库时不排除）：测试里放了 30 MB 的被忽略文件，包只有 12 KB。远程模式下每个实例有自己的服务令牌（job id 不同，只能以自己的 job 创建 artifact，不再是整个 run 共用一个；同一 run 的别的 artifact 仍然读得到，GitHub 也是这样）。runner 快照可以自动重做：`make runner-binary VERSION=x` 编静态二进制，`raccoonci runner-snapshot [-binary …] [-replace 旧id]` 起一台干净的 `universal`、装二进制和 agent 服务、检查版本和静态链接、拍快照、删机器（实测 11 s）。**限制**：机器上的日志和主控离得远时没有压缩；`run` 时还不校验快照清单里的 `raccoonci` 版本和模板版本。API 有 `DELETE /v1/snapshots/{id}`（key 需要 `images:delete`），所以旧快照可以由我们自己删，不必手工
- ⬜ **起机名额**（和 A.3 同一件事）：InstaBox Pro 每分钟最多起 10 台，A 路径的矩阵 fan-out 和 B 路径每个 job 一台机器都会撞到；用户在查能否提高。A 里的 `starts.go` 和服务端的 `instabox.StartLimit`（C.1 第 6 步）都只是在名额内排队，不能提高名额
- ⬜ 自有引擎改用 `ci` 模板做 runner 快照（原 B.2 搁置项里的一条）：改 `BuildRunnerSnapshot`，装静态 `raccoonci`、agent 的 systemd unit 和清单，端到端重跑 `ci.yml`，并重新检查 catalog 里的 Debian 假设（预装内容见 B.2 的搁置段）
- ⬜ Boat.dev
- ⬜ DigitalOcean / Linode
- ⬜ 不可用 OS 的兜底：现在只有"明确失败 + plan 提前警告"；"连接自己的 Mac / Windows 机器去跑"还没做
- ⬜ Windows 支持（可能以后放在 InstaBox 上）；macOS 只能走用户自己的机器
- ⬜ 沙箱：job 现在直接以当前用户在本机运行，隔离完全依赖 provider 的虚拟机

## C.3 基准与调度（自有引擎在 InstaBox 上；已移到 https://github.com/Raccoon-CI/raccoonbench）

项目做完后用来和现有方案比时间与花费，用法见新仓库的 README。下面的记录写于移出之前，路径 `cmd/raccoonbench`、`bench/` 现在在新仓库里。serve 路径（A）的基准见 A.2。

- ✅ 已做（2026-10-29）：`raccoonbench run` 按"系统 × 工作负载"跑，每个会话从零开始依次跑三个场景（冷启动、同一提交再来一次、改一个文件后的提交），`-repeat N` 次取中位数和范围；系统有四种：当前版本的 raccoonci（`-on instabox` 每个 job 实例一台机器，可带 `-env-snapshots`、`-affected`）、**更早版本**的 raccoonci（`ref` 是本仓库的 git 提交，现编译，放在一台 InstaBox 机器上整个 workflow 一台机器跑，就是"更新前的自己"）、GitHub 托管 runner（fork 上 `workflow_dispatch`，读 API 里的 job 起止时间和标签）、InstaBox 官方 runner（同一个 fork 的另一个分支，`runs-on: instabox-medium`）。**结果里只存原始用量**（秒数、机器规格、每个 job 的计费时长、保存了几个快照），**不存钱**；`raccoonbench report -prices bench/prices.yml` 才按价格表算，所以涨价降价了只要改价格文件（带 `as_of` 和来源）重新出报告，不用重跑。计费规则：InstaBox 机器按"请求创建到删除返回"的秒数 × 规格单价（vCPU、内存、磁盘）+ 保存的环境快照在保留时间内的存储；GitHub 每个 job 向上取整到分钟 × 标签单价（标签没价格就报"未知"，不瞎猜）；InstaBox 官方 runner 按秒 × 价格文件里写的规格（"medium"到底多大要你填）。报告每行还核对"做的是不是同一批 job"（因不受影响而略过的 job 算成功），避免靠少做活得到的提速。产品里新增：`run -on instabox` 结束时打印机器数、机器秒数和估算花费，`-usage-file` 写出每台机器的请求/就绪/删除时间和规格；还打印 `results ready after X s`（出结果的时间，不含保存快照和删机器的收尾）。顺带修了 `splitRemote` 的一个 bug：`-affected` 这类无值的开关会吞掉后面一个词。价格文件里放了你给的 InstaBox 单价（2026-10-29 价格页）和 GitHub 每分钟 $0.008（当时没核对；后来已改成 2026-10-31 读到的当前价，见 A.2）。
- 🟡 实测过的部分：本机直接跑、旧版本在一台机器上跑、当前版本 `-on instabox`（含快照、affected）。GitHub 托管和 InstaBox 官方 runner 两个系统当时**只用假服务器测过**（派发、等待、读 job、取整计费）；后来 A.2 的几轮基准已经对 GitHub 托管和 InstaBox 官方 runner 做了真实测量。
- **第一次真实测量的结果（`monorepo` 夹具，每个测试睡 15 秒，各 2 次，当时的 runner 快照是旧二进制）**：旧版本一台机器：冷 39 s、warm 17 s、change 17 s，约 $0.0002–0.0004；当前版本每个 job 一台机器：冷 55 s、warm 45 s、change 48 s，约 $0.0008–0.0009，**更慢也更贵**，因为每台机器的启动（几秒到二十秒，并发创建时更长）比 15 秒的测试还长，deploy 又要再等一台机器。`-affected` 在 change 场景略过 frontend，花费降到 $0.0005，时间基本没变（backend 和 deploy 的机器启动占大头）。环境快照在这个夹具上没有用上（子目录里的 `go.mod`/`package.json` 让它被判为"不能快照"）。结论：**每个 job 一台机器只有在 job 本身够长、或并行度够高时才划算**，job 只有几秒到十几秒的工作负载上不如一台机器跑完。这是后面要正视的设计问题（比如：短 job 合并到同一台机器、机器池/预热、从快照启动前提前创建）。
- ✅ 短 job 合并到同一台机器（2026-10-29）：`internal/sched` 把每个实例在机器上的耗时（从开始到最后一步，不含机器启动）按"workflow + 实例名"记下最近 5 次，取中位数；估计值低于机器启动时间（本次运行已测到的中位数，没有就按 10 s）的 3 倍就算短。短实例交给 `instabox.Pool`：先填满已有（或正在启动）的机器再开新的，每台机器同时跑的数量默认等于 vCPU 数（`-pack-slots N`，1 表示一个接一个），空闲机器留 15 s 给紧接着就绪的下游 job，run 结束时全部删除。`-no-pack` 关闭。没有历史的实例算长（独占一台机器），所以第一次运行和以前一样，第二次起才合并。用到 `container:`/`services:` 的 job 和能用环境快照的实例不合并。runner 快照里的 agent 现在是 `raccoonci agent -loop -slots 8`：每个槽位一个配置文件（`dispatch.SlotConfig`），读完就删，跑完等下一份。**需要重建 runner 快照**（旧快照的 agent 只跑一个任务）。真机结果（`monorepo` 夹具，同一台沙箱发布控制器，新 runner 快照 `sn_EXAMPLE_SNAPSHOT_ID`，各跑 1 次）：第 1 次（无历史）3 台机器、48 s、$0.0008；第 2 次 2 台、37 s、$0.0008（backend 上次 32.7 s 没低于 30 s 的线，独占）；第 3 次 1 台、34 s、$0.0004。每个 job 一台机器时是冷 55 s / $0.0009。**还没做的**：没有用 `raccoonbench` 跑完整的冷/warm/change 三场景、也没重复取中位数，所以和"更早版本一台机器"的 17 s warm 比还没有结论；每台机器上的 Go 模块缓存是冷的，这是和那台长期机器的主要差别；并行槽位里的 job 若绑定固定端口会互相冲突（目前只排除了 container/services）；预热机器池没做，等有真实使用数据。
- ✅ 合并短 job 之后的复测（2026-10-29，提交 `9c482bc`，新 runner 快照 `sn_EXAMPLE_SNAPSHOT_ID` 是从这个提交建的，agent 单元里设了 `TMPDIR=/data/home/tmp`，因为 `/tmp` 是 2 GB 的内存盘而一台机器可能同时放几个 job 的源码）：`raccoonbench`，`monorepo` 夹具（两个 15 秒的测试 + deploy），每个系统 3 个会话，中位数（范围）。**更早版本一台机器**（`d814e16`）冷 39.9 s、warm 17.5 s、change 17.3 s，约 $0.0002–0.0004；**每个 job 一台机器**（`-no-pack`）冷 62.8 s、warm 60.4 s（54–139，有一次机器创建特别慢）、change 43.0 s，约 $0.0008–0.0010；**合并短 job** 冷 62.4 s、warm 48.1 s（34–51）、change 33.0 s（32–41），约 $0.0007–0.0009，机器数从每次 3 台降到 warm 平均 2.3 台、change 平均 1.7 台。结论：合并有效（warm 快约 20%，change 快约 23%，花费略降），但**这个小夹具上还是比更早版本慢 2–3 倍、贵 3–5 倍**。要公平地看这个差距：①基准里"更早版本"的那台机器在三个场景间一直开着，warm 和 change 不含机器启动，也不含它在两次运行之间的空闲费用（真实使用里这台机器要么一直开着花钱，要么每次重新建，启动 6–20 s 就又回来了）；②当前版本第一次（冷）没有历史，全部独占；③每台机器的 Go 模块缓存是冷的。**下一步值得做的**：预热机器池（把启动摊掉）、让 deploy 这类依赖前面 job 的短 job 直接复用刚跑完的机器（现在要等 linger 内的新实例才用得上）、历史只记独占机器上的耗时（现在合并后的并行争抢 CPU 会拉长耗时，可能把一个 job 推过"短"的线再退回来）。
- ✅ 环境快照的键认工作区（2026-10-29）：npm/pnpm/yarn、Cargo 工作区的成员没有自己的锁文件，由上层目录的那一份锁文件覆盖，所以 JS/Rust 的 monorepo 现在可以快照（键里是根目录的锁文件）；不在工作区里的子目录项目、没有锁文件的清单仍然判为"不能快照"。Go 的多模块工作区还是每个模块要有自己的 `go.sum`。

---

# D. 拆仓库的计划（2026-11-02 写；**先别动手拆**，等你说开始）

用户 2026-11-01 定：服务端（C.1）补齐后，拆成 **frontend / backend / runner / cli** 四个仓库，外加一个**共用库**仓库；独立仓库、独立发版，代码开源、多人维护。C.1 现在补齐到下表"还剩什么"为止，所以这份计划可以执行。下面的划分是我读了 `go list` 的真实依赖得出的，不是凭印象；**要你拍板的地方单独列在最后**。

## D.1 现状：谁用谁（`go list` 实测）

- `cmd/raccoonci` 一个二进制干了所有事：本机运行（`run`）、规划（`plan`/`match`/`affected`）、GitHub 触发路径（`serve`，`internal/ghrunner`）、机器上的 `agent` 和 `run-instance`、做 runner 快照（`runner-snapshot`）、`catalog update`、`runner-images update`。拆分就是把它按角色分开。
- `cmd/raccoonci-server` + `internal/server/*`（api、artifacts、auth、caches、config、execute、github、imagesync、instsync、intake、oidc、retention、schedule、secrets、storage、storagecheck、store、vault）是 backend。它从服务端之外只拿这些：`blob`、`workflow`、`runnerimages`、`artifact`、`trigger`、`instabox`、`dispatch`、`catalog`、`wire`、`value`、`runner`、`expr`、`engine`、`cron`、`cache`、`archive`。
- **两条缝挡在拆分前面**：①backend（`execute`）用 `engine.Run` + `Options.Remote` 来排作业图，而 `engine` 同时 import 了 `provision`、`scan`、`actions`、`runner`、`missing`……（这些是机器上才需要的重家伙）；②`wire`（库里的协议）import 了 `runner` 只为拿 `Event`/`Result`/`Conclusion` 这几个类型。不先切这两条，库会把整个 runner 拖进去，backend 也会。

## D.2 每个包去哪（建议）

| 仓库 | 内容 |
|---|---|
| **共用库**（`lib`，名字待定） | 纯数据与协议，没有"执行"：`value`、`expr`、`workflow`、`trigger`、`cron`、`semver`、`affected`、`catalog`（含 `catalog.yaml`）、`platform`、`runnerimages`（含内置快照）、`archive`、`twirp`、`blob`、`azblob`、`artifact`、`cache`（artifact/cache 的协议实现：backend 和本机 `run` 都要）、`wire`、`dispatch`（协议和主控一侧；agent 一侧的逻辑在 runner）、`instabox`（API 客户端和 Provider；backend、cli、ghrunner 都用）、`envsnap`、`sched`、`plan`、**以及从 `runner` 里切出来的事件类型**（见 S2）和**从 `engine` 里切出来的作业图**（见 S1） |
| **backend**（`backend`） | `cmd/raccoonci-server` 和 `internal/server/*`；`openapi.yaml`（前端的契约）；服务端的文档（`server-dev`、`server-design`、`github-app`）；迁移 SQL |
| **runner**（`raccoonci-runner`） | 机器上跑的东西：`agent`、`run-instance`、`runner`（执行 step 的）、`actions`、`scan`、`provision`、`nodejs`、`docker`、`missing`、`mirror`、`local`（本机 checkout 等）、`engine` 里"跑一个实例"的那一半；静态二进制的构建（`make runner-binary`）、`runner-snapshot` 的制作 |
| **cli**（`cli`） | `raccoonci` 命令：`run`（本机引擎，会依赖 runner 仓库的执行部分）、`plan`、`match`、`affected`、`catalog`、`runner-images`、`serve`（见 D.5 的问题 3）；`ghrunner` |
| **frontend**（`raccoonci-frontend`） | 现在**没有任何代码**。只靠 backend 的 `openapi.yaml`（JSON over HTTP，日志用 SSE，登录用 `/v1/auth/github/login?redirect=` 回到前端、会话在 URL 片段里）。技术栈待定 |
| 已经独立 | `raccoonbench`（基准） |

## D.3 动手前必须先在本仓库里做的重构（每一步 `make check` 过、单独提交）

- **S1 把 `engine` 切成"作业图"和"跑一个实例"两半**：作业图（needs、matrix、fail-fast、`max-parallel`、`if`、outputs、动态 matrix、`Options.Remote`、`RemoteInstance`）只依赖 `workflow`/`expr`/`value`，进库；"跑一个实例"（`RunInstance`、扫描、装环境、选镜像、调 `runner`）留在 runner。本机的 `raccoonci run` 用"图 + 本地实例执行器"拼起来。这是最大的一步，风险在 `engine.go` 的 `runInstance` 里图和执行缠在一起；现在 `engine.go` 里还有另一个会话没提交的改动（`Options.Missing`、`OnMissing`），**动它之前要先等那边提交**。
- ✅ **S2 把 `runner.Event`/`Result`/`Conclusion`/`EventKind` 挪到不依赖执行的包**（2026-11-02 做完）：新包 `internal/runevent` 只放这些数据类型和常量（只依赖 `value`），`runner` 里改成类型别名和常量别名，所以所有 `runner.Event`、`runner.Success` 的老写法不变；`wire` 已直接改用 `runevent`。守护测试：`wire` 的传递依赖只能是 `runevent` 和 `value`（变异检查：让 `wire` 重新 import `runner`，测试失败并列出被拖进来的 9 个包）；`runevent` 只能 import `value`；事件类型的编号和结论的字符串固定（它们在线上跑着旧二进制的机器之间传）。**还没改的**：backend 的 `internal/server/execute`、`local`、`cmd/raccoonci` 仍写 `runner.Event` 等（别名保证能用）；backend 对 `runner` 剩下的依赖是 `runner.Config`（`engine.Options.Runner`），要等 S1 把 `engine` 切开才能去掉。
- ✅ **S3 `dispatch` 里 agent 一侧与主控一侧分清、协议有版本号**（2026-11-02 做完）：核对后 `dispatch` 本来就按文件分了（`agent.go` 机器一侧，`controller.go` 主控一侧，`tar.go` 共用），所以真正缺的是**版本号**，已加：`dispatch.ProtocolVersion = 1`、请求头 `X-Raccoonci-Protocol`（agent 每个请求带自己的，主控每个回复带自己的），`wire.Version` 和 `InstanceSpec.Protocol`（任务的版本）。规则写在 `dispatch/protocol.go` 和 `docs/server-design*.md`：只加可选内容不升版本，改含义或删东西才升，主控支持 `MinProtocolVersion` 到自己的所有版本；**没带版本的老 agent/老主控算 1**（已部署的 runner 快照就是这样，所以不用重建快照）；比主控新的 agent 被拒绝，**这个 run 立刻以原因结束**而不是等机器超时；`run-instance` 拒绝比自己新的任务并说明"快照比主控旧，要重建"。单测：dispatch 5 个（两边都说 1、老 agent 无头照常服务、新 agent 被拒且 run 带着原因结束、新主控服务老 agent、头不合法 400）、真二进制 1 个、执行器断言每个任务带版本。**没做**：agent 一侧在主控比自己旧时的处理（现在没有比 1 更旧的，所以没有可测的情形）；`dispatch` 的客户端（`Agent`）仍和 `Controller` 在同一个包里，拆仓库时按 D.2 它们一起进库（协议在库、agent 的进程在 runner）。
- **S4 改 module 路径**：现在是 `raccoonci`，各仓库要真实的路径（`github.com/Raccoon-CI/…`）；`internal/` 在跨仓库后不能再 import，所以库里要导出的包要从 `internal/` 挪出来（`pkg/` 或顶层），并想清哪些是对外稳定的 API、哪些仍是内部。
- 🟡 **S5 `cmd/raccoonci` 按角色拆成几个 `main`**。**第一步已做（2026-11-02）**：试验表明机器一侧的文件几乎没有缠在 cli 里（分开后 runner 一侧只缺一个 `listFlag`），所以把 `agent.go`、`instance.go`、`runenv.go`、`runnersnap.go` 和 `agent_test.go` 用 `git mv`（历史保留）搬进新包 `internal/runnercmd`（导出 `Agent`、`RunInstance`、`Snapshot`、`NewRunEnv`/`RunEnv`/`ValidInstall`），共用的 `listFlag` 抽成 `internal/cliflag.List`；`cmd/raccoonci` 的 `main.go` 改为调用它们，`remote.go` 直接用 `wire.*`（原来经 `instance.go` 里的别名）；新增**很薄的** `cmd/raccoonci-runner`（`agent`/`run-instance`/`runner-snapshot`/`-version`，只用内置 catalog，已加进 `make build`）。两个二进制共用同一份代码，`TestRunInstanceOverTheWire` 对两个都跑。**没动的**：`cmd/raccoonci/run.go` 和 `serve.go`（另一个会话还有未提交的改动），所以 `cmd/raccoonci/runenv.go` 留了一个 5 个字段的**适配层**（`runEnv` 镜像 `runnercmd.RunEnv`），`run.go` 一行没改；`run.go` 里还有自己的 `listFlag`。**还要做**：①等 `run.go` 空出来，删适配层和重复的 `listFlag`；②按决定 #8 把 `run`（本机引擎）也搬进 runner 的二进制——现在 `raccoonci-runner` 没有 `run`，而 agent 的 `Job.Command` 还允许 `run`（基准的"旧版本一台机器"用它），所以**现有 runner 快照仍安装 `raccoonci`**，没改；③`CLAUDE.md` 的布局说明要补一行 `cmd/raccoonci-runner` 和 `internal/runnercmd`（那个文件也有别的会话的未提交改动，没碰）。

## D.4 搬的顺序和办法

1. S1–S5 全部做完并提交，`make check` 绿。
2. 用 `git filter-repo --path …`（或 `git subtree split`）从本仓库**保留历史**地切出库仓库；库先发 `v0.1.0`，带 `CHANGELOG`。**注意一段坏历史**：提交 `0c3ca27` 到 `45906d6`（含）之间，`internal/engine/engine.go` 因为我一次错误的按块暂存而无法编译，`421c793` 才修好；这一段里的提交单独检出会编译失败（`git bisect` 会踩到）。拆分时要么接受并在库/runner 仓库的说明里写明，要么在切历史前把 `421c793` 的修复用 `git rebase`/`fixup` 并回去（这会改写已有提交的 id，要你同意再做）。
3. runner、backend、cli 依次切出，各自 `go.mod` 依赖库的固定版本（开发期可以用 `go.work` 把几个仓库连起来一起改）。
4. 每个仓库自己的 CI：用 Raccoon CI 跑自己（`.github/workflows/ci.yml`，`make check`），backend 加上"`openapi.yaml` 和路由一致"的测试（已有）。
5. 文档：沿用"英文 `x.md` + 中文 `x.zh.md`"；`TODO.md` 按 A/B/C 切：A（GitHub 触发路径）→ cli，B（自有引擎）→ runner，C.1 → backend，C.2（provider）→ 库或 backend，C.3 已在 raccoonbench。`CLAUDE.md` 每个仓库一份，各写各的。
6. 最后一步才删本仓库里的旧目录；这之前每个仓库都要能独立 `make check`。
7. 版本和兼容：每个仓库独立 semver；**协议有自己的版本号**（`dispatch` 控制器↔agent、`wire`/`InstanceSpec`、backend 的 `/v1` API）；已经在用的"旧机器忽略新字段"的做法（`InstanceSpec.Images`）要成为规则：协议只加可选字段、不改旧字段含义，不兼容的变化升大版本并让 backend 同时支持上一版的 agent。

## D.5 要你定的（我先给了建议，但这些是产品/组织层面的）

> **2026-11-02 16:14 用户授权：需要拍板的事按 TODO 和既定原则选最稳妥的方案、记下理由、继续做（花钱、删用户别的资源、不可逆的除外）；真正拆成独立仓库仍要等用户确认。** 下面每一条的"决定"就是据此选的。它们**只影响拆分前的准备工作**，没有一个是不可逆的，用户随时可以推翻。
>
> | # | 决定 | 理由 |
> |---|---|---|
> | 1 | 作业图进库（S1：切成两半） | 唯一不让 backend 依赖 runner 的做法；D.7.3 实测 backend 只用 `engine` 的 3 个名字，所以代价小 |
> | 2 | 仓库名暂用 `lib`/`-backend`/`-runner`/`-cli`/`-frontend`，放在 GitHub 组织 Raccoon-CI 下，module 路径 `github.com/Raccoon-CI/raccoonci-<角色>`；**许可证：Apache-2.0（用户 2026-11-02 定，已闭环，见 D.8）** | 名字和路径只是占位，真拆时一个 `go.mod` 改动；许可证由用户定，不是我定的 |
> | 3 | `serve` 和 `ghrunner` 先放 cli | CLAUDE.md：两条路径都不删、谁是主线未定；放 cli 不关闭任何一条路，以后搬进 backend 只是移动 |
> | 4 | `instabox` 放库的 `provider/instabox` | 现在只有一个 provider；第二个出现再拆，过早拆是白添一个仓库 |
> | 5 | 前端技术栈**不选**，现在不写前端 | 没有任何代码依赖这个决定；backend 是纯 JSON API（`openapi.yaml` 是契约），前端以后能用任何栈；先做会白做 |
> | 6 | 公开 API：只承诺 `value`、`workflow`、`expr`、`trigger`、`catalog` 的数据结构和 `wire`/`dispatch` 协议；其余写明不稳定（0.x） | 承诺越少以后越自由；协议已经有版本号（S3） |
> | 7 | 集成测试用环境变量开关、默认跳过 | 和现有 `TestReal*` 一致；不让别的贡献者必须有 InstaBox 账号才能 `make check` |
> | 8 | 本机 `raccoonci run` 放 runner 仓库的二进制，cli 做面向平台的命令 | D.7.4：这样 cli 对 runner 的依赖最小；可逆（命令在哪个仓库只是搬文件） |
>
> 下面是原来的问题和当时的建议，留作依据。

1. **作业图放哪**：建议 S1 的"切成两半、图进库"。备选：整个 `engine` 放 runner，backend 依赖 runner 仓库（简单，但 backend 会背上 `provision`/`scan` 这些重依赖，也违背"backend 不依赖 runner"）。
2. **库的名字、仓库组织和 module 路径**（许可证已定为 Apache-2.0，见 D.8）。
3. **`serve`（GitHub 触发路径、`ghrunner`）放 cli 还是以后做成 backend 的一个模块**：CLAUDE.md 说两条路径哪条是主线还没定、都不删。建议先放 cli（它现在就是个命令），backend 想吸收时再搬。
4. **`instabox` 放库还是单独的 provider 仓库**：DigitalOcean/Linode/Boat.dev 加进来后会有多个 provider；建议现在先放库的 `provider/instabox`，等第二个 provider 出现再决定是否拆。
5. **前端技术栈**，以及它是否也要由 backend 静态托管。
6. **`internal` → 公开 API 的边界**（S4）：哪些包承诺稳定。建议只承诺 `value`、`workflow`、`expr`、`trigger`、`catalog` 的数据结构和 `wire`/`dispatch` 协议，其余仍写明"不稳定"。
7. **开发环境**：隧道（`ci.example.com`）、开发 App、InstaBox runner 快照现在都为这一个仓库配的；拆后每个仓库的测试用什么配置（我建议集成测试都用环境变量开关，和现在的 `TestReal*` 一样，默认跳过）。
8. **cli 和 runner 的界线**（D.7.4）：本机 `raccoonci run` 放 runner 仓库的二进制里（建议），还是 cli 仓库？

## D.6 C.1 补齐后还剩什么（拆分不用等它们，但要带着）

- ⏸ catalog feed（等你定，见 C.1 那一条）。
- 浏览器里真走一遍 GitHub 登录（先在 App 里登记回调地址）；真 PR 的投递、matrix、真实 artifact 上传、机器上的 step 真用 `GITHUB_TOKEN`/`getIDToken`——这些都要往仓库里加 workflow 文件，而 gh token 没有 `workflow` 权限（`gh auth refresh -s workflow`）。
- 真桶（R2）的访问密钥，S3 路径才算真测过。
- （用量额度已做，见 C.1 的"用量与额度"一条；还缺的是按仓库各自不同的额度和快用完时的提醒。）
- 没有云厂商接受过我们签的 OIDC token。
- 前端。

## D.7 S4 的依据：谁真的用了哪些导出名（2026-11-02，实测，不动代码）

方法：写了一个小的 AST 统计工具（会话临时目录里，没入库；约 150 行，数每个 `internal/<包>` 的导出名，并按"将来属于哪个仓库"数每个名字被包外的哪些仓库用到；`cmd/raccoonci` 按文件名分：`agent.go`、`instance.go`、`runenv.go`、`runnersnap.go` 算 runner，其余算 cli；不含测试文件）。**注意它的局限**：①方法不计入（只数 `pkg.Name`）；②一个类型只出现在别的函数的签名里（调用者不写它的名字）会被算成"包外没用"，其实不能取消导出（例如 `plan.Plan`、`affected.Decision`、`workflow.Strategy`）；所以"没用"只是**候选**，要逐个看。

### D.7.1 对 D.2 的修正（按真实使用）

| 包 | D.2 里放在 | 实测谁用 | 改成 |
|---|---|---|---|
| `semver` | 库 | 只有 `provision`（runner） | **runner** |
| `affected` | 库 | 只有 cli（`cmd/raccoonci` 的 `affected`/`run -affected`、`envsnap`），backend 没用 | **cli** |
| `envsnap` | 库 | 只有 cli（`run -on instabox -env-snapshots`） | **cli** |
| `plan` | 库 | 只有 cli（`plan` 命令）；它又用 `scan`、`actions`（runner） | **cli**（所以 cli 依赖 runner 仓库） |
| `sched` | 库 | cli 和 `ghrunner`，backend 没用 | **cli** |
| `local` | runner | cli（`run`）和 runner（`instance.go` 的 checkout）都用 | 留 **runner**，cli 依赖它 |
| `azblob`、`twirp` | 库 | 只有库里的 `artifact`/`cache` 用 | 留在库里但**不公开**（库的内部包） |
| `cache`、`artifact`、`blob`、`cron`、`instabox`、`dispatch`、`wire`、`trigger`、`catalog`、`runnerimages`、`archive`、`platform`、`runevent`、`value`、`expr`、`workflow` | 库 | 至少两个仓库用 | 库，**公开**（见 D.7.2） |

依赖方向因此是：**库 ← runner ← cli**，**库 ← backend**；backend 不能依赖 runner。现在 backend 通过 `engine` 和 `runner` 间接依赖 runner，这正是 S1 要切断的。

### D.7.2 库里要公开的包，和包外真正用到的名字数

`value`（8 个导出，8 个被用）、`expr`（12/11）、`workflow`（9/7，没人用：`MaxMatrixJobs`、`Strategy`）、`trigger`（6/5）、`catalog`（**37/16**：21 个包外没用，多是 `*Def`/`*Rule` 这类类型，在 `Catalog` 的字段里，需要保留）、`runnerimages`（7/5）、`runevent`（19/19）、`wire`（7/6）、`dispatch`（11/6：`ProtocolVersion` 等只在包内）、`instabox`（22/12：10 个包外没用，`CreateRequest`/`MachineInfo`/`Operation` 是 API 载荷类型）、`blob`（16/13）、`artifact`（10/9）、`cache`（5/5）、`cron`（2/2）、`platform`（2/2）、`archive`（2/2）。结论：**表面积不大**，库的公开 API 合计约 190 个导出名，其中 `catalog` 和 `instabox` 最值得在公开前瘦身（把只是数据载荷的类型留着，把纯内部的函数改成小写）。

### D.7.3 S1 的目标接口很小（backend 到底用了 engine 和 runner 的什么）

- backend 用 `engine` 的**只有三个名字**：`Run`、`Options`、`RemoteInstance`。`Options` 里它设的字段：`Remote`、`Github`、`Vars`、`Inputs`、`Images`、`Catalog`、`WorkDir`、`MaxParallel`、`Runner`（只用 `OnEvent`）。所以进库的"作业图"只需要提供这三个名字，`Options.Runner` 换成一个 `OnEvent func(runevent.Event)` 就不再需要 `runner.Config`。
- backend 用 `runner` 的：事件类型和结论常量（已经是 `runevent` 的别名，**可以机械地改成 `runevent.X`**，约 80 处，我没做是因为只能逐处编辑），以及 `runner.Config`（上面说的）。
- runner 一侧只用 `dispatch.Agent`、`dispatch.SlotConfig`、`engine.RunInstance`、`engine.Options`、`instabox.RunnerSpec`（制作快照）。
- 这三点合起来说明 S1 比 D.3 里担心的要窄：**backend 只依赖一个很小的"作业图 + 远程实例"接口**。

### D.7.4 这次发现、要你定的

- **cli 和 runner 的界线**：cli 的 `run`（本机引擎）、`plan` 都要用 runner 仓库的 `engine`、`runner`、`scan`、`provision`、`actions`、`local`、`missing`，也就是说 cli 实际依赖 runner 的大半，runner 仓库里这些包也得公开。**建议**：本机 `raccoonci run` 属于 runner 仓库的二进制（"能执行 workflow 的那个程序"），cli 仓库做面向平台的命令（对 backend 的 API 说话：看 run、日志、触发，外加 `plan`/`match`/`affected`/`catalog`/`serve`）。这样 cli 对 runner 只剩 `plan`（用 `scan`），依赖更清楚。备选：保持现在的"一个 `raccoonci` 命令全都有"，只是放在两个仓库里实现。写进了 D.5 的问题 8。
- 统计工具本身：想入库的话建议放 `tools/apiscan`，拆分前后都能用来检查"有没有越界依赖"。

## D.8 许可证：Apache-2.0（用户 2026-11-02 定）

- ✅ 仓库根目录有 `LICENSE`：标准 Apache-2.0 全文，与发行版自带的 `/usr/share/common-licenses/Apache-2.0` **逐字节相同**（`cmp` 和 sha256 核对过，11358 字节、202 行）。镜像（`deploy/Dockerfile`）加了标准标签 `org.opencontainers.image.licenses="Apache-2.0"`（`docker build --check` 通过）。
- ✅ **版权人和年份（2026-11-02 用户定了，"折中方案"）**：`LICENSE` 附录填 `Copyright 2026 Steven Zhao`，只改了这一行。用户只说了填附录，所以**没加**文件头和 `NOTICE`（下一条）；若"折中方案"还包括别的（例如只给入口文件加头），等用户说明。以下是之前的分析：Apache-2.0 并不要求把它填进 `LICENSE` 文件本身——它只是"怎么把许可证用到你的作品上"的示范——真正要写版权人的地方是：①各文件头的版权声明（可选，Apache 建议但不强制）、②`NOTICE` 文件（只在有需要保留的署名声明时才需要，现在没有，所以没建）。用户给出版权人（个人还是组织、写哪个名字）和起始年份后再决定写哪些地方。
- 没加文件头：几百个文件逐个加会很吵，也没法替用户决定版权人；等上一条定了再说。
- **依赖的许可证核对**（从 `go list -m all` 的模块缓存里读 `LICENSE`，粗分类）：Apache-2.0 14 个、BSD 24 个、MIT 4 个，没有 GPL/AGPL/LGPL。例外：①`github.com/hashicorp/golang-lru/v2` 是 **MPL-2.0**（文件级 copyleft），经 `modernc.org/sqlite` 进入服务端二进制；未修改地使用，与 Apache-2.0 兼容，不要求我们的代码也用 MPL，但**发布二进制或镜像时要附带第三方许可证声明**（见下）；②`github.com/kr/pretty`、`kr/text` 的模块里没有 LICENSE 文件，但它们不在本仓库任何二进制或测试的构建里（只在别的模块的测试依赖图里），所以不影响。
- **还要做**：①发第一个版本前生成第三方许可证声明（`THIRD_PARTY_NOTICES`，把依赖的版权声明和许可证文本汇总；工具如 `go-licenses`，这台机器没装，且要联网装，没做）；②拆仓库时每个仓库放一份同样的 `LICENSE`；③`docs/gha-compat.md` 里说 act、Gitea 的代码可以"带署名地引用或复用"：我们目前只是参考没有拷贝代码，若以后拷贝，要保留它们的版权声明（MIT 要求），并写进 `NOTICE`。
