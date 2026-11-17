# 团队知识助手

面向"基于 goagent 开发的团队成员"的知识助手：回答 goagent 与 goagent-contrib 的文档问题并给出来源路径，按用户记住偏好与决定，清空记忆、重建索引这类操作须人工审批。两个实例共用一套存储，任一实例都能接收请求。

这个目录有两个用途。一是给外部开发者一份"用 goagent 做真实产品"的样板：一条 `docker compose up` 就能部署并长期运行。二是在真实模型、多实例和崩溃恢复下检验框架。组装过程中暴露的框架问题记在 [FINDINGS.md](FINDINGS.md)，产品里不绕过。

下文按"怎么搭一个这样的产品"组织。设计依据见 [规格](../../docs/superpowers/specs/2026-11-16-knowledge-assistant-design.md)。

## 架构

```
浏览器 / curl ── Authorization: Bearer <用户名>
   │                         │
   ▼                         ▼
 实例 a :8081 (WORKER=1)    实例 b :8082 (WORKER=0)
 ├─ /v1/*      management/httpapi：会话、任务、审批、运行查询
 ├─ /api/*     产品接口：确保 Agent、我的记忆（搜索/删除）、来源查看
 └─ /          静态页面：对话、待审批、我的记忆
   │  两个实例都只把命令持久化到 Postgres；只有持有 Worker 槽位的实例执行任务
   ▼
 ┌──────────────── Postgres 17 + pgvector ────────────────┐
 │ management/pgstore  会话、任务、审批、Worker 槽位        │
 │ durable/pgstore     会话日志、长期记忆记录与抽取任务      │
 │ memory/pgvector     记忆向量索引                         │
 └─────────────────────────────────────────────────────────┘
 Redis 7        模型调用缓存（cache/redis，按命名空间与会话打标签）
 Qdrant 1.20    文档向量（retrieval/qdrant），alias ka_docs 指向当前集合

一次性服务：migrate（建表）→ ingest（采集 /corpus 写入 Qdrant）→ 才启动 a、b
```

一次提问在 Worker 上的执行顺序：

1. management 取出任务，经 `NativeFactory` 构建 Agent。
2. 产品把任务租户绑定为记忆命名空间。
3. Builder 检索 4 条文档证据，召回该用户的长期记忆，查模型缓存，未命中才调用模型。
4. 模型可以调用记忆工具。`purge_my_memory` 和 `rebuild_docs_index` 会让任务停在 `waiting_approval`，任一实例上批准后由 Worker 续跑。

## 组件选型与理由

| 能力 | 选用 | 为什么 |
| --- | --- | --- |
| 会话、任务、审批、恢复、HTTP | 核心 `management` + `adapters.NativeFactory` + `management/httpapi`，存储用 contrib `management/pgstore` | 审批状态机、续跑命令与决定的原子持久化、崩溃后的启动恢复，框架都已提供。产品自己写就是在复制框架逻辑。contrib `httpagent` 只有 invoke/stream，没有审批续跑入口 |
| 对话模型 | `internal/chat`（拷贝自仓库的 `internal/livemodel`），DeepSeek / MiniMax / Gemini 按 `GOAGENT_LIVE_PROVIDER` 选择 | 与仓库其它真实模型案例保持一致。Go 的 internal 包不能跨 module 导入，所以只能拷贝（F8） |
| 嵌入 | Gemini `gemini-embedding-001`，启动时探测维度 | 文档和记忆共用一个嵌入器，换模型时两边一起失效、一起重建。provider 不声明维度（F6），只能探测 |
| 文档切分 | contrib `document` 的 `RecursiveCharacterTextSplitter`；Markdown 按标题切成小节，Go 包文档用 `go/doc` 解析导出注释 | 来源要精确到小节，标题必须由产品自己保留。Markdown/Token 切分器经 `SplitDocuments` 不会切分（F7） |
| 文档检索 | `retrieval.VectorStoreRetriever` + `retrieval/qdrant`，经 `builder.WithRetriever` 接入 | RAGRetriever 不实现 `DocumentRetriever`，接不上 Builder（F11） |
| 文档向量库 | Qdrant，alias 蓝绿切换 | 不改框架即可开工，也顺带验证现成的 Qdrant 模块。代价是多一个服务、向量分两处存。全部放进 Postgres 是后续工作 |
| 长期记忆 | goagent `memory`：Durable = `durable/pgstore`，Index = `memory/pgvector`，抽取由 `Memory.Run` 异步完成；脚本模型下没有抽取，改跑 `Memory.RunMaintenance` | 两个实例各持有一个 `memory.Memory`，读写同一套存储。跨实例一致靠共享存储，不依赖 Worker。框架要求不跑记忆 Worker 的部署必须跑维护，否则 Purge 中断后的旧作用域、Forget 残留的向量无人清理；维护各步幂等，两个实例都跑 |
| 模型缓存 | `builder.ModelCache`：`cache/redis` + `Erase: EraseRequired` + `OnErase: Memory 的 Eraser` | 记忆删除后，用到它的缓存条目由框架回调擦除，产品不直接删 Redis 键。`EraseRequired` 让"后端不支持擦除"在构建时就失败，而不是线上静默保留 |

### 取值理由

- **切分 800 字节、重叠 100 字节**：`document` 的切分器默认用 `len` 计长，单位是字节不是字符（F27）。中文一字 3 字节，800 字节约合 266 个汉字，英文和代码约 800 个字符。中文小节短的落在一块里，来源能精确到小节；长的会切成数块，但来源仍指向同一小节。100 字节的重叠（约 33 个汉字）让被切开的那句话在相邻两块里都有完整出现的机会。改按字符计长会重新切分全部语料，现有验收结果随之作废，所以保留字节口径。
- **语料范围**：完整语料约 4200 块。goagent 的 `docs/superpowers`（`specs` 与 `plans`）整体不收录：这些文件带日期，描述的是当时的意图，真实嵌入下却排在模块文档前面（见 FINDINGS 验证状态）。代价是丢掉只写在设计文档里的细节，例如 `durable/pgstore` 的具体建表 SQL：它只出现在 specs 和 `schema.go` 的常量值里，而 godoc 不包含常量值。问到这类细节时，回答会如实说明文档未给出。
  语料目前还收录了给 AI 编码助手看的工作流文件，如 `CLAUDE.md`、`.github/*.md`，它们会被检索到并被回答引用。排除它们会改变块数、使现有真实验收作废，暂不改动（见 FINDINGS 产品侧已知限制）。
- **召回 4 条**（`RetrievalConfig{MaxDocuments: 4}`，`/api/sources` 同样取 4）：问题通常落在一两个小节，4 条足以覆盖"概念 + 用法"两类证据。再多会挤占上下文，模型也更容易引用不相干的路径。
- **缓存 TTL 4 分钟**：`memory` 默认的 `EraseWithin` 是 5 分钟，`EraseRequired` 要求 TTL ≤ EraseWithin，否则构建失败。这样即使擦除回调因故没有执行，旧条目也会在承诺的擦除时限内过期。
- **Gemini 嵌入 8 并发，可重试**：provider 没有批量接口，串行嵌入完整语料约需一小时。实测 10 并发约每秒 10 条且未被限流，所以默认取 8 留出余量，可用 `KA_EMBED_PARALLEL`（1–64）调整。provider 每次调用只发一个请求、不重试（F25），产品对单条嵌入最多尝试 5 次，失败后按指数退避重试，等待窗口依次为 1、2、4、8 秒（全抖动，在窗口内随机取值）。代码里 30 秒的单次上限在 5 次尝试下达不到。服务端给出 Retry-After 时按它等待，超过 1 分钟则放弃。重试只针对 429、5xx、暂时性连接错误和单次请求超时，否则一次限流就会让整个 ingest 失败，a、b 也就起不来。

## 启动

前置：Docker 与 Docker Compose 2.23 或更高版本（`docker-compose.yml` 用 `configs.content` 内联 Postgres 初始化脚本，这是 2.23 才支持的写法），以及 Gemini 嵌入 key 和一个对话模型 key。

```bash
cd products/knowledge-assistant
cp .env.example .env        # 填 KA_CACHE_TAG_KEY、GEMINI_API_KEY 与对话模型 key；.env 不入库
docker compose up -d --build
```

- 首次构建会克隆 goagent `v0.54.1` 和 contrib `396d3eca4884` 作为语料。来源路径对应确定的提交，重建索引的结果因此可复现。
- `ingest` 用 Gemini 嵌入约 4200 块，需要几分钟。`a`、`b` 等它成功结束后才启动。
- 页面在 http://127.0.0.1:8081 和 http://127.0.0.1:8082，填任意小写用户名即可。端口被占用时设 `KA_A_PORT`/`KA_B_PORT`。
- 端口只绑定 127.0.0.1，因为身份方案没有真实认证（见下文）。

不想调用付费 API 时，可以用离线模式演练部署与故障转移：

```bash
KA_MODEL=script KA_EMBEDDER=hash KA_CACHE_TAG_KEY=$(openssl rand -hex 32) docker compose up -d --build
```

脚本模型只按固定规则回显，不代表产品效果。

清理：`docker compose down -v`，须在与启动相同的环境变量下执行（compose 对 `KA_CACHE_TAG_KEY` 做必填插值，缺了连 `down` 也会报错）。项目名固定为 `knowledge-assistant`，不会碰到其它 compose 项目或测试容器。

### 子命令

镜像里只有一个 `ka` 二进制：

| 命令 | 作用 |
| --- | --- |
| `ka migrate` | 建齐全部表，可重复执行，多实例并发启动时由 advisory 锁串行 |
| `ka ingest` | 采集 `/corpus` 写入 Qdrant。幂等键由语料内容和嵌入模型共同决定，语料没变就不重写，换了嵌入模型必定重建。与经审批的 `rebuild_docs_index` 共用一把 Postgres advisory 锁，同时发生时排队执行 |
| `ka serve` | HTTP 服务；`WORKER=1` 时同时运行 Worker |
| `ka recover-worker -reason <原因> -confirm-stopped` | 回收已停止实例占着的 Worker 槽位 |

### 测试与验收

支持的平台：

- 离线测试（`scripts/test.sh`）支持 Linux 和 macOS，需要 Docker、bash、curl。双实例 e2e 用了 Linux 专有的 `Pdeathsig`，文件带 `//go:build linux`，在 macOS 上不运行，其余包照常运行。
- 运维脚本（`live.sh`、`failover.sh`）支持 Linux 和 macOS，另需 jq、perl，以及 `uuidgen`（没有时退回 Linux 的 `/proc/sys/kernel/random/uuid`）。
- Windows 未验证。

- 离线：`scripts/test.sh ./...`。脚本临时启动 Postgres、Redis、Qdrant 容器，跑单元测试和双实例 e2e（规格第 6 节场景 1–6），用脚本模型和 3072 维哈希嵌入。参数同时传给 `go vet`，所以 `-v` 之类的标志要放在包路径前面。设置 `KA_TASKSET`（如 `KA_TASKSET=2,3,6-27`）时，`go test` 经 `taskset -c` 绑定到这些 CPU 核上运行，仅 Linux 可用。
- 真实模型：`KA_LIVE=1 scripts/live.sh`，用真实对话模型和 Gemini 嵌入跑场景 1–3：
  - 3 个文档问题，必须引用期望的仓库路径前缀；
  - a 上写入记忆，b 上新会话召回；
  - 经 b 删除记忆，a 不再召回，召回时产生的缓存条目被物理删除。

  任一条件不满足即以非零退出。报告写入 `verification/live-<UTC时间>.json`，包含每题的回答、来源、召回、token 用量（取自 `GET /v1/runs/{task}`）和耗时。

## 多实例语义与故障转移

management 规定全局只有一个活动 Worker，既不分片，也不在超时后自动接管。原因是 Worker 持有任务的执行租约：两个 Worker 同时续跑同一个审批后的任务，工具副作用就会执行两次。所以：

- a、b 都提供完整 HTTP API。命令先持久化到 Postgres，由持有槽位的 Worker 执行，因此在 b 上提交或批准的任务也由 a 执行。
- 记忆和缓存的跨实例一致不依赖 Worker：两个实例的 `memory.Memory` 读写同一套存储，同一个 `KA_CACHE_TAG_KEY` 保证缓存标签一致。
- a 和 b 都不设重启策略。a 被强杀后槽位仍归它所有，自动重启的 a 也拿不回槽位；该由人判断 a 是否真的停了。

a 宕机时运行：

```bash
scripts/failover.sh "a 所在主机宕机"
```

脚本按框架规定的流程执行：

1. `docker compose kill a`，并确认 a 不在运行。仍在运行就退出 1，因为回收槽位等于声明"旧实例已停止"。
2. `ka recover-worker` 读出当前持有者的 owner 与 generation，以运维身份调用 `RecoverWorkerSlot`，原因写入审计。
3. `WORKER=1 docker compose up -d --no-deps b` 以 Worker 身份重建 b，由 b 执行启动恢复：停在待审批的任务批准后由 b 续跑，已完成的不重复执行。
4. 在 b 上跑一个探测任务，直到它成功。框架有个缺陷：Worker 认领槽位途中进程收到 SIGTERM，槽位会泄漏，而进程仍以 0 退出（F19）。所以脚本要等 b 确实持有槽位后才返回，返回之后再重启 b 才安全。

注意事项：

- 运行 `failover.sh` 时的环境变量（或 `.env`）须与 `docker compose up` 时相同，否则 b 会以不同的模型或嵌入配置重建。
- 故障转移之后，不要不带 `WORKER=1` 直接 `docker compose up -d`：那样会把 b 重建为非 Worker 并重新拉起 a。`failover.sh` 只做 a→b 方向。要切回 a，按下面的步骤手工执行：

  ```bash
  docker compose kill b
  docker compose ps -q --status running b   # 须为空，b 仍在运行就不能回收
  docker compose run --rm --no-deps a ka recover-worker -reason "切回 a" -confirm-stopped
  docker compose up -d --no-deps a          # a 的 WORKER 固定为 1
  ```

  然后在 a 上提交一个任务（例如在页面上问一句"你好"），等它成功，确认 a 已持有槽位（F19）。之后再执行 `docker compose up -d --no-deps b`，把 b 重建为非 Worker；此时 shell 里不能带着 `WORKER=1`。

这一步需要人工介入，是框架有意的设计：只有运维能确认旧进程确实不在了。目前不顺手的地方在于槽位持有者只能直接读内部文档（F2），而且 owner 是 uuid，对应不到实例（见 FINDINGS）。

## 文档索引丢失时的恢复

alias `ka_docs` 不存在或指向的集合被删后，每个任务构建 Agent 时都会报"索引 ka_docs 不存在，请先运行 ka ingest"。长期记忆、会话与审批都存在 Postgres 里，不受影响，重新采集即可：

```bash
docker compose run --rm ingest     # compose 部署
ka ingest                          # 直接运行二进制时，环境变量与 serve 相同
```

ingest 与审批触发的重建共用一把锁，在线时也可以执行。重建期间 a、b 无须重启：检索器绑定的是 alias 名，切换后的下一个任务就会读到新集合。

## 生产须替换的部分

- **认证**：`Authorization: Bearer <用户名>` 只是演示。令牌就是用户名，任何人都能冒充任何人。生产环境要在 `web.ActorFromRequest` 处接入真实身份（OIDC、网关注入的身份头等），映射出 `Actor.TenantID` 与 `Subject`，同时去掉端口只绑 127.0.0.1 的限制。
- **租户模型**：会话不记录创建者，产品只能让每个用户独占一个租户（F10）。团队内共享会话需要框架支持后再调整。
- **审批不是授权**：每个用户独占一个租户，`TenantAuthorizer` 只核对租户，所以审批项只能由发起人自己批准。审批只防模型误调用工具，不限制谁能执行。任何用户都能让 `rebuild_docs_index` 执行。语料和嵌入模型未变时重建直接复用现有集合；一旦变化，就是一次全量重新嵌入，约 4200 次 Gemini 调用。框架目前表达不了"由管理员审批"（F28）。生产环境须实现区分管理员身份的 Authorizer，或不向普通用户开放这个工具。
- **运维身份**：`recover-worker` 以固定的 `ops` Actor 调用，内置的 `TenantAuthorizer` 对这个系统级操作不设防（F20）。若要经 HTTP 暴露回收能力，须先实现区分运维身份的 Authorizer。
- **密钥**：`KA_CACHE_TAG_KEY` 和模型 key 应来自密钥管理系统，不要放在 `.env`。
- **重启策略**：交给编排系统时，非 Worker 实例可以自由重启；Worker 实例被强杀后须走故障转移，不能盲目重启。
- **文档向量**：Qdrant 是为了尽快开工的选择，后续计划改存 Postgres（见 FINDINGS 后续工作）。
