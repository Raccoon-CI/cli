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
- ⬜ artifact 剩余（搁置，见本节末尾）：元数据持久化（现在在内存，重启就忘）；retention 到期清理（`expires_at` 只存不执行）；整体 sha256 校验（现在只记录）；在真实 AWS 上验证 S3（目前只测过 SeaweedFS）；`download-artifact` 的按 ID 跨 run 下载（需要 GitHub token）
- 🚫 artifact v3（REST）：不做。GitHub 已弃用 v3，用 v3 的 workflow 应该升级到 v4
- ✅ cache v1（`/_apis/artifactcache`）：`internal/cache`。真实 `actions/cache@v4` 与 `cache/restore@v4` 实测（20 MB、1 MiB 分块、两个 job；第二个进程命中第一个进程存的缓存）。缓存按仓库放在用户缓存目录，跨次运行保留
- ✅ cache 淘汰与大小上限：`-cache-limit-gb`（默认 10，和 GitHub 一样，0 不限）超了就淘汰最久没用的，单个条目超过上限直接拒绝；提交前按 `statfs` 检查磁盘剩余，不够就先淘汰、仍不够返回 507。`internal/cache/limit_test.go` 有 v1/v2 的单测
- ✅ cache 放 S3 兼容存储：`-cache-s3-bucket`（凭据走 `RACCOONCI_CACHE_S3_KEY/_SECRET/_TOKEN`），条目放桶里，客户端从桶的预签名链接下载。对着 SeaweedFS 的 S3 网关验证过，没有试过真实的外部桶
- ⬜ cache 剩余（搁置，见本节末尾）：按分支隔离（现在同仓库所有 ref 共用）；多个 `raccoonci` 进程同时写同一个 index（后写覆盖）；下载链接与 run 共用同一把密钥
- ✅ cache v2（Twirp `CacheService`：CreateCacheEntry / FinalizeCacheEntryUpload / GetCacheEntryDownloadURL）：`internal/cache/v2.go`，上传那一半抽成了 `internal/azblob`（新增单次 Put Blob）。`raccoonci run -cache-protocol v2` 下用真实 `actions/cache@v4` 实测：20 MB（单次 Put Blob，第二个进程命中）、150 MB（超过 SDK 单次上限 128 MiB，走分块），各跑两个 job。v1 与 v2 共用同一批缓存条目
- ⬜ cache v2 剩余：默认协议仍是 v1（toolkit 只有设了 `ACTIONS_CACHE_SERVICE_V2` 才走 v2，服务端不会按 toolkit 版本自动选）；请求里的 `CacheMetadata`（repository_id、scope）被忽略；150 MB 那次没有确认分块走的是直通路径还是落盘路径
- ⬜ OIDC token 端点（`ACTIONS_ID_TOKEN_REQUEST_URL`）
- ⬜ 真实的 `GITHUB_TOKEN`（GitHub App installation token，调用真实 GitHub API）
- ⬜ job summary 的展示与 annotations 持久化：`runner.Result.Summary` 收集了 `$GITHUB_STEP_SUMMARY` 的内容（上限 1 MiB），但没有任何地方读它，本机运行也不打印。用户在自有引擎下看不到 summary

**搁置：单机之外才需要**：artifact 元数据持久化、retention 清理、整体 sha256 校验、cache 按分支隔离、多进程写同一个 index、下载链接单独的密钥、本机运行的按 job token。它们都只在多人共用一台 controller 或长期运行的服务里才会出问题；现在 `raccoonci run` 是单次运行、单进程。做长期服务（C.1）时再捡起来。

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
- ⬜ `concurrency`（group / cancel-in-progress）
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

**当前目标（用户 2026-11-01）：先写服务端，写完再按四个仓库拆。** 代码在 `internal/server/…` 和 `cmd/raccoonci-server`，按"以后可以直接搬走"的规则写（只依赖将来进库仓库的包，对外都走自己定义的小接口）。工作顺序见设计文档：1 骨架和入口 ✅ → 2 执行 🟡（本机和 InstaBox 上都跑通了一个 run；matrix、真实 artifact 等没验证） → 3 回报 🟡（commit status、取消、重跑已做；check run 没做） → 4 artifact/cache 落库 🟡（cache 按空间隔离已做并在真机验证；桶和 artifact 元数据落库没做） → 5 密钥与认证 🟡（密钥、变量、角色做了；GitHub 登录和每组织一个桶没做） → 6 定时与 dispatch → 7 加固。

- ✅ 第 1 步（2026-11-01，**未提交**）：SQLite 存储和编号迁移（`store`）、验签的 webhook（`intake`：HMAC-SHA256 常量时间比较，空密钥一律拒绝，不在允许列表的仓库不入库，投递 id 去重，先存后答 202，后台读 workflow、`trigger.Match`、建 run，重启后补处理）、GitHub 读取和 App 认证（`github`：JWT 换 installation token 并缓存）、只读 API（`api`：`/v1/runs`、`/v1/runs/{id}`、`/v1/events/{id}`，bearer token，没配 token 就 503，`/v1/openapi.yaml`）、密钥读取（`config`：环境变量或 600 权限的文件）、`raccoonci-server replay`。单测都在（含 `-race`）。**真实验证**：把 `cli/cli` 一次真实 push 的签名回放经 Cloudflare Tunnel（`ci.example.com`）发给服务端，用真实 GitHub API 读到 15 个 workflow，建了 3 个 run，和 GitHub 自己在那个提交上跑的三个完全一致；paths 过滤按真实变更文件跳过了一个；重复投递回答 duplicate。**没试过**：GitHub 自己发出的投递（用户要自己建 webhook 或 App，我创建仓库 webhook 被权限拦下，没有绕开）、真实 PR、App 认证对真实 GitHub（只测了假服务）。
- 🟡 第 2 步（2026-11-01，**未提交**）：执行（`internal/server/execute`）。执行器用 `Store.ClaimRun` 原子地领 run，取源码（GitHub 的归档重新打包成 `src/` 一层，`github.Client.SourceTar`）、用 `engine.Run` + `Options.Remote` 把每个 job 实例交给一台机器（`Machine` 接口：`Instabox` 适配 `instabox.Provider`；`Local` 在服务端这台机器上跑 `raccoonci run-instance`，**没有任何隔离**，只用于开发和可信代码，`-local-runner`），机器回传的事件（`internal/wire`，从 `cmd/raccoonci` 里抽出来的 controller↔runner 协议）批量存成这个 run 的日志；服务重启时进行中的 run 记为失败，关机时取消正在跑的；artifact 服务（`internal/server/artifacts`，token 绑定到某个 run 和 job，没有 cache 服务，因为 cache 必须按仓库和分支隔离）；`POST /v1/runs` 手动启动 `workflow_dispatch` 的 workflow（输入按声明检查）；API 加了 `/v1/runs/{id}/jobs` 和 `/v1/runs/{id}/log`（分页，或 SSE，`Last-Event-ID` 可续传）；规格和路由有一个测试保证一致。**真实验证**：`-local-runner` 下，经隧道 `POST /v1/runs` 启动 `bench-lint-docs` 的 workflow，服务端取到真实归档，用真的 `raccoonci run-instance` 跑了 checkout、`setup-node`、三个 `run` 步骤，存了 62 行日志，job 成功，API 读到；一共 4 秒；SSE 经隧道实时推送也通了。**InstaBox 上也跑通了**（runner 快照 `raccoonci-runner-9c482bc`，机器经隧道的 `/agent` 回连）：同一个 workflow 成功，机器 9.8 秒就绪，从请求到结束 16 秒，机器事后已删除（`IBX_API_KEY` 其实在，只是只在登录 shell 里，我的 Bash 工具不是登录 shell，用 `bash -lc` 就有）。**没验证**：matrix、artifact 在真实 job 里、真实 PR、GitHub 自己发出的投递。**已知限制**：PR 用的是头提交不是合并提交；源码里没有 `.git`；没有密钥（`secrets.GITHUB_TOKEN` 为空）；没有 cache 服务；并发起机没有顾及 InstaBox 每分钟 10 台的限额（`serve` 里有，服务端还没接）；`github.run_number` 就是 run id；用 App 认证时手动启动的 run 没有 installation id（`Token(0)` 会报错）
- 🟡 第 3 步（2026-11-01，**未提交**）：回报、取消、重跑。每个 run 在它所跑的提交上设 commit status `raccoonci / <workflow 名>`（开始 `pending`，结束 `success`/`failure`/`error`），用 token 和 App 都行（`-status=false` 关闭）；`POST /v1/runs/{id}/cancel`（排队的立刻结束，运行的取消并删机器）、`POST /v1/runs/{id}/rerun`（同一提交、当时的 workflow 文本、同样的输入，`rerun_of`）。**真机验证**：GitHub 上提交的状态先 pending 后 success；5 秒时取消的 run 为 cancelled、状态 error；重跑成功。**发现并修了一个真泄漏**：创建机器过程中取消，删除被 409 `operation_conflict` 拒绝后就放弃，机器会一直跑到 2 小时期限（`instabox.Client.DeleteMachine` 现在等待并重试，真机验证没有残留；那台漏掉的机器 `rc-r5-1` 我已删）。**没做**：check run（每个 job 一行、注解、summary，只有 GitHub App 能写）；也没从 GitHub 发出的投递建出过 run（token 没有 `workflow` 权限，没法加 push 触发的 workflow；需要你 `gh auth refresh -s workflow` 或自己加一个）。**webhook**：已在 `bench-lint-docs` 建了仓库 webhook（id 706032788，指向 `https://ci.example.com/webhooks/github`，push 和 pull_request），ping 回 200，两次 push 和一个 PR 的打开关闭都被收到并记为已忽略；测试分支已删，PR #2 已关（GitHub 不能删 PR）；调试结束要删这个 webhook
- 🟡 第 4 步（2026-11-01，**未提交**）：缓存（`internal/server/caches`）和对象存储。cache 按"空间"隔离：默认分支一个空间（只有默认分支的 run 写），其他每个 ref 各一个空间，能读自己的和默认分支的，不能读别的 ref、不能写默认分支；仓库之间不共用；fork PR 同样走自己的 ref 空间（所以不能给默认分支投毒）；按 token 里的 run 路由到各自的 `cache.Service`，只通告 v1。对象放 `-data-dir` 下，或放用户自己的 S3 兼容桶（`-storage-s3-*`，R2 用 endpoint `https://<账号>.r2.cloudflarestorage.com` 加 region `auto`），启动时检查桶（写、用预签名链接读回、删；桶对匿名请求公开读则警告），桶不能用就拒绝启动。**真机验证**：`bench-go-integration` 的 `setup-go cache: true`，第一次保存 44 MB，重跑恢复，新分支上的 run 通过默认空间恢复，磁盘上只有默认空间；隔离规则的测试在去掉隔离后会失败。**没验证**：桶（S3 路径只用假的测过，没有 R2 的访问密钥——你需要在控制台建一个只管 `raccoonci-dev` 的 API 令牌，key 和 secret 放进 `RACCOONCI_STORAGE_S3_KEY/_SECRET` 的文件）、真实 job 上传 artifact。**没做**：按组织配置的桶（等密钥存储，第 5 步）、artifact 元数据落库和保留期、没人用的空间的清理（GitHub 是 7 天没命中就删）、按仓库的总大小上限
- 🟡 第 5 步（2026-11-01，**未提交**）：密钥、变量和 API token 角色（没有 GitHub 登录）。主密钥 `RACCOONCI_MASTER_KEY` 用 AES-256-GCM 加密密钥后才进数据库（绑定 scope 和名字，轮换用 `RACCOONCI_MASTER_KEY_OLD`），`secrets` 服务按 scope（`org:<名>`、`repo:<owner>/<名>`）保存；run 只拿到 workflow 里按字面写出的 `secrets.NAME`，仓库的盖过组织的；fork 的 PR 拿不到任何密钥（日志里写明），变量都能拿到；密钥只写不读，列表只有名字。API 的路由分 open/read/admin 三档，`read` token 只能读，改动类和密钥、token 管理要 admin（否则 403），token 只存哈希、只在创建时显示一次、可吊销。**验证**：用真正的 `run-instance` 跑（`RACCOONCI_TEST_BIN=dist/raccoonci`）密钥打印成 `***`、日志里任何地方都没有、fork 的是空的；经隧道真实调了存取和 403、吊销。**没做**：每个组织一个桶（存储设置是组织的一条密钥；artifact 要像 cache 按空间那样按组织路由）；用 GitHub 登录（需要 App 的 OAuth，现在没有 App）；按仓库限制 token 能看什么。发现并修了一个我自己引入的回归：先把"没有 `Bearer ` 前缀的头"也当成 token，测试抓到了
- 🟡 每个组织一个桶（2026-11-01，**未提交**）：`PUT /v1/storage` 给一个组织自己的 S3 兼容桶（R2、S3 都行），保存前先试（写、预签名链接读回、删），不能用就拒绝并说明哪一步失败，对匿名公开读的桶警告；设置和密钥用主密钥加密，放在组织 scope 下名字 `$STORAGE`、kind `storage` 的一行（`$` 不可能出现在密钥名里），workflow 引用不到、密钥 API 列不出也改不了。该组织各仓库的 artifact 和 cache 之后进它的桶（cache 空间按桶版本重建，artifact 按 run 所属的组织路由到各自的服务）；没配的用服务端自己的（`-storage-s3-*` 或本机目录）。**验证**：单测（含路由、版本变化、取不到桶时 500 而不是悄悄落到本机）；经隧道真实调了一次，指向不存在的主机的桶被拒绝、没有保存、密钥没有出现在返回里。**没验证**：真实的桶（R2 的访问密钥还没给）。
- 🟡 数据库：事件、run、job、日志已有；artifact/cache 元数据、密钥、installation、runner-images 与 catalog 数据还没有
- 🟡 API：run、job、日志（含 SSE）、事件、手动启动；缺重跑、取消、密钥、认证登录；UI（frontend）可以开始了
- ⬜ 把状态回写 GitHub（checks / statuses）（第 3 步）
- ⬜ secrets / vars 的加密存储
- ⬜ 凭证和机密放数据库（服务端版本）：现在缓存桶的密钥只从环境变量读（`cmd/raccoonci/cachestore.go` 是唯一读环境变量的地方，`blob.S3Config` 已经是"一个桶的配置"，服务端按组织/仓库从库里取出后 `blob.NewS3` 即可，缓存服务不用改）；workflow 的 `-secret` 也是本地环境变量。服务端要做：信封加密入库（主密钥来自 KMS 或服务启动配置，读出后只在内存用，日志和 API 响应里不出现）；按组织/仓库隔离，不同租户不同桶或不同前缀；能用短期凭证（STS）就不存长期密钥，并支持轮换；机器永远拿不到桶密钥，只拿预签名链接；workflow 机密只在派发任务时解密，并和"InstaBox Org Secret 占位符代理换头"一起设计，让机器上根本没有真值
- ⬜ 工作目录清理与磁盘配额（现在 workdir 只增不减）
- ⬜ 服务端定期轮询 runner-images（ETag + 存库），目前只有命令行手动更新
- ⬜ 来自 fork PR 的 workflow 的 secrets 策略
- ⬜ catalog feed：默认地址（还没发布，**不能编一个**）、签名生成流程和工具
- 依赖这里的 B 项：OIDC 和真实 `GITHUB_TOKEN`（B.3 未决项）、artifact/cache 的持久化与按分支隔离（B.3 搁置项）、内容哈希的按需执行（B.6 ③）、完整的 `github` 上下文（B.5）

## C.2 算力提供方（provider）

优先级：用户自己的机器 → InstaBox → Boat.dev → DigitalOcean/Linode。

- 🟡 机器上的 agent 和主控协议（`internal/dispatch`、`raccoonci agent`）：机器主动向主控发起 HTTPS 连接，领取任务、拉源码包、回传日志和心跳、报告退出码；不走 provider 的 exec（InstaBox 全账号 32 并发的限额），机器不需要公网地址。每个任务一个随机令牌（主控只存它的哈希，令牌只开自己的那个任务），日志按偏移量上传所以重试不重复、有缺口会拒绝；机器第一次联系超时（2 分钟）或之后 30 秒无声都会让 `Run.Wait` 报错（机器死了能发现）；agent 只接受 https（或回环地址的 http），因为令牌和机密都走这条连接；配置在 `/etc/raccoonci-agent.json`（令牌不放命令行）。用真实二进制测过：源码包过去、`actions/checkout` + `run` 步骤成功、失败退出码回传、机密在日志里被遮蔽。**还没做**：主控的对外进程和域名（开发期可用 `instabox-port` 发布，地址公开所以每个请求都鉴权，且只在这台机器运行时有效）；产物（artifact）和缓存在机器上跑完就丢，没有回传；机密现在随任务明文发到机器（要换成 Org Secret 占位符，或至少限定为机器专属令牌）；一次只一个任务，机器用完即删
- ⬜ 用户自己的机器（agent 连接）：协议已有，缺注册和长驻模式
- 🟡 InstaBox（Debian 13）：代码已写（`internal/instabox`：API 客户端 + `Provider`；`raccoonci run -on instabox -listen ADDR -controller-url https://… -snapshot ID`，密钥读 `INSTABOX_API_KEY`），一台机器跑整个 workflow。流程：从快照建机器（带 `deleteAfterSeconds` 兜底，主控进程死了服务端也会删）→ 等操作完成 → 文件接口写 `/data/home/.raccoonci-agent.json`（令牌）→ 快照里的 systemd 服务 `raccoonci-agent` 读到后连主控 → 跑完删机器（取消也会删）。API 规格来自 `https://api.instabox.sh/v1/openapi.json`（公开，能查全部接口）。新快照 `raccoonci-runner-agent-23db1cb`（`sn_EXAMPLE_SNAPSHOT_ID`）带 agent 服务；机器内 home 是 `/data/home`，不是 `/home/user`。**真实端到端已跑通（2026-10-29）**：本项目自己的 `ci.yml`（checkout、setup-go、gofmt、`make check`、build、upload-artifact）用 `-on instabox` 在新建机器上成功，job 用时 1m15s，整个命令 1m35s（含建机器、传源码、删机器），机器事后已不在列表里；主控通过 `instabox-port` 发布的地址被机器连上。API 要求所有写操作带 `Idempotency-Key`（客户端每次随机生成）。密钥读 `INSTABOX_API_KEY`，没有则读 `IBX_API_KEY`（`INSTABOX_API_KEY` 是机器上的保留名，org Secret 只能用别的名字；shell 里是占位符，代理换头）。**现在是一个 job 实例一台机器（2026-10-29）**：引擎在主控这边解析 needs 和矩阵（`engine.Options.Remote` 收到每个已解析的实例），每个实例发给一台新机器，机器上 `raccoonci run-instance` 用 `engine.RunInstance` 跑（装环境、选镜像、runner 都在机器上），事件按 JSON 行流回来、结果带 outputs 回来（`wireReader`，`value.FromJSON` 保持键顺序），所以 `needs.*.outputs`、动态矩阵、fail-fast、`max-parallel` 都照常。artifact 和 cache 服务由主控用公开地址提供（`publicServe`，链接和环境变量都用 `-controller-url`），机器直接访问，所以 job 之间的产物/缓存通了。实测 cobra 全部 workflow：10 台机器并行（lic-headers、golangci-lint、8 个 ubuntu 矩阵），整条命令 56 s，ubuntu 实例各 9–18 s，8 个 macOS 和 1 个 Windows 实例按设计在主控这边直接失败，没有占机器；10 台并发创建时机器就绪 6–22 s（单台约 6 s），事后全部删除。runner 快照现在是 `raccoonci-runner-ri-4008bd4`（`sn_EXAMPLE_SNAPSHOT_ID`），旧的两个已用 API 删掉。`-parallel` 在远程模式默认 8。**收尾（2026-10-29）**：源码包不再带 git 忽略的文件（`.git` 保留；不是 git 仓库时不排除）：测试里放了 30 MB 的被忽略文件，包只有 12 KB。远程模式下每个实例有自己的服务令牌（job id 不同，只能以自己的 job 创建 artifact，不再是整个 run 共用一个；同一 run 的别的 artifact 仍然读得到，GitHub 也是这样）。runner 快照可以自动重做：`make runner-binary VERSION=x` 编静态二进制，`raccoonci runner-snapshot [-binary …] [-replace 旧id]` 起一台干净的 `universal`、装二进制和 agent 服务、检查版本和静态链接、拍快照、删机器（实测 11 s）。**限制**：机器上的日志和主控离得远时没有压缩；`run` 时还不校验快照清单里的 `raccoonci` 版本和模板版本。API 有 `DELETE /v1/snapshots/{id}`（key 需要 `images:delete`），所以旧快照可以由我们自己删，不必手工
- ⬜ **起机名额**（和 A.3 同一件事）：InstaBox Pro 每分钟最多起 10 台，A 路径的矩阵 fan-out 和 B 路径每个 job 一台机器都会撞到；用户在查能否提高。A 里的 `starts.go` 只是在名额内排队，不能提高名额
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
