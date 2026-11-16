# 团队知识助手（实战项目）设计

日期：2026-11-16
位置：`goagent-examples/products/knowledge-assistant`（独立嵌套模块）

## 1. 意图

两个目的，缺一不可：

1. **示范**：给外部开发者一个"用 goagent 做真实产品"的完整样板。它是能部署、能长期运行的服务，不是单一能力的演示。
2. **验证**：在真实模型、真实部署、多实例、崩溃恢复下检验 goagent 与 contrib。v0.54 的长期记忆与分布式后端、核心 `management` 管理服务，至今只在测试或单一案例中用过。产品组装中暴露的框架问题是本项目的产出之一，要记录下来，不在产品里绕过。

## 2. 产品

面向"基于 goagent 开发的团队成员"的知识助手：

- **文档问答（RAG）**：知识库为 goagent 与 goagent-contrib 的 Markdown 文档和 Go 包文档注释。回答必须附来源，给出仓库相对路径与小节，例如 `goagent-contrib:durable/pgstore/README.md#建表`。
- **长期记忆**：按用户记住偏好与决定，例如"我们用 pgstore""我偏好中文示例"，后续对话自动召回。
  - 用户可在页面上查看自己的记忆、删除单条记忆。这是产品自己的 REST 接口，内部调用 `memory.Forget`。
  - 也可以在对话里说"忘掉这条"，由 Agent 工具调用 `Forget`，这个操作不需要审批。
- **审批**：两类敏感操作做成 Agent 工具，执行前须人工批准：
  - 清空当前用户的全部记忆（`Purge`）；
  - 重建文档索引。

  审批走 `management` 的 Approval：任务停在待审批，经 HTTP `DecideApproval` 批准或拒绝后续跑，决定与续跑命令原子持久化。
- **入口**：
  - 后端：核心 `management/httpapi` 的标准 HTTP API，覆盖会话、任务、审批、运行查询。
  - 产品额外挂两组接口：记忆查看与删除，以及来源查看。
  - 前端：一个最小静态页面（对话、待审批列表、我的记忆），参考 `orchestration/httpagent/web` 的写法，只调用上述 HTTP 接口。
- **用户身份**：页面填写用户名，请求带 `Authorization: Bearer <用户名>`。产品实现 `management.Authorizer`，把它映射为用户 ID，同时用作记忆命名空间与会话归属。这里不做真实认证，README 写明生产环境须替换。

## 3. 目标、非目标、反目标

**目标**

- 一条 `docker compose up` 起全套：Postgres 17（含 pgvector）、Redis 7、Qdrant、两个助手实例 `a`、`b`，以及一次性的 `ingest` 服务。前面不加负载均衡，两个端口分别访问。
- 可重复的验收场景（见第 6 节）。离线部分进 `go test`；真实模型部分进带开关的脚本，脚本带断言并产出报告。
- README 按"怎么搭一个这样的产品"组织，每个组件说明为什么这样选。

**非目标**

- 真实认证、多租户计费、前端美化。
- 文档以外的数据源。
- 增量同步。重建索引是全量的、经审批的操作。
- 多实例自动接管执行。框架明确不支持，见第 5 节。

**反目标**（技术上能跑，但出现即算失败）

- 在产品代码里复制或绕过框架逻辑，例如自写审批状态机、自写任务恢复。遇到框架缺口，在 `FINDINGS.md` 记录，修框架或明确标为已知限制。
- 只在模拟模型下验证，真实模型路径从未跑过。
- 两个实例之间状态不一致而没有被测出：一个实例删掉的记忆，另一个实例还能召回或命中缓存。

## 4. 方案

讨论过两种文档向量存储：

| 方案 | 部署 | 取舍 |
| --- | --- | --- |
| **A. 文档向量存 Qdrant（已选）** | Postgres + Redis + Qdrant | 不改框架、立刻开工；顺带验证现成 Qdrant 模块。多一个服务，向量分两处存 |
| B. 先在 contrib 新增 pgvector 版 `interfaces.VectorStore`，全部放 Postgres | Postgres + Redis | 部署最简、补框架缺口；但要先设计、测试、发布一个 contrib 模块，范围约翻倍 |

用户选 A。"文档向量也放 Postgres"列为后续工作，写进 `FINDINGS.md`。

服务层选型：最初设想用 contrib `httpagent`，核对后发现它只有 invoke/stream/ws/webhook，没有审批与续跑入口。改用核心 `management`，它本就负责会话、任务、审批、恢复。产品不另造这些。

## 5. 架构

| 能力 | 组件 |
| --- | --- |
| 运行管理、会话、任务、审批、HTTP | 核心 `management` + `management/adapters.NativeFactory` + `management/httpapi`；存储用 contrib `management/pgstore` |
| 对话模型 | `internal/livemodel`（DeepSeek / MiniMax / Gemini 按环境变量选） |
| 嵌入 | Gemini 原生 provider 的 `Embed`，`GEMINI_API_KEY` + `EmbeddingModel`（默认 `gemini-embedding-001`，3072 维）。文档与记忆共用这一个嵌入器 |
| 文档加载与切分 | contrib `document`：Markdown 加载器 + 切分器。Go 包文档由 ingest 用 `go/doc` 解析导出的包注释与导出符号注释，转成文本后同样切分 |
| 文档检索 | contrib `retrieval` 的 RAG 检索器 + `retrieval/qdrant`，经 `builder.WithRetriever` 接入 Agent |
| 长期记忆 | goagent `memory`：`Durable` = contrib `durable/pgstore`，`Index` = contrib `memory/pgvector`，抽取按 `memory` README 推荐方式 |
| 模型调用缓存 | `builder.ModelCache`：`Cache` = contrib `cache/redis`，`OnErase: mc.Eraser()`，`Erase: EraseRequired`。删除记忆后的缓存擦除由框架回调，产品不自己删 Redis 键 |

**多实例语义**：`management` 规定只能有一个活动 Worker，不做多实例分片，也不做超时自动接管。

- a、b 都提供 HTTP API。命令持久接纳到共享的 Postgres，所以任一实例都能接收对话、审批、记忆操作。
- 只有一个实例运行 Worker，默认是 a，由环境变量 `WORKER=1` 指定。
- **记忆与缓存的跨实例一致**不依赖 Worker：两个实例各自持有 `memory.Memory`，读写同一套 pgstore、pgvector、Redis。
- **故障转移**按框架规定的流程，由脚本 `scripts/failover.sh` 执行：
  1. 确认 a 已停止；
  2. 调用 `ConfirmWorkerStopped`，带上 owner、generation 和原因；
  3. 以 `WORKER=1` 重启 b，由 b 执行启动恢复。

  这一步是人工或运维动作，README 写明原因。如果实际用下来不顺手，在 `FINDINGS.md` 记录。

**知识库来源**：`ingest` 镜像构建时按固定版本 `git clone` 两个仓库：goagent 用 tag `v0.54.1`，contrib 用 master 的固定提交。来源路径为"仓库名:仓库相对路径#小节标题"。

**重建索引**：

- 写入新的 Qdrant collection，名字带时间戳；完成后切换 alias，再删除旧 collection。
- 重建期间问答继续读旧 alias。
- 重建只由 Worker 执行。单活动 Worker 加上审批的"恰好一次"语义，已经排除了并发重建。

**依赖版本**：

- goagent `v0.54.1`。
- contrib 各模块用当前最新 tag：`durable/pgstore/v0.5.0`、`document/v0.3.0`、`retrieval/v0.4.0`、`retrieval/qdrant/v0.4.0`、`cache/redis/v0.1.0`、`memory/pgvector/v0.1.1`；`management/pgstore` 按其 go.mod 的发布情况取用。
- 只用已推送的提交，不用本地 replace，独立克隆即可构建。
- 如果某模块与 goagent v0.54.1 不兼容，先修 contrib 并发版，再继续，同时记入 `FINDINGS.md`。

## 6. 验收场景

**离线（`go test`）**：用本地模拟模型与确定性嵌入器（3072 维，按词哈希）；外部服务由 docker 测试脚本启动，跟 contrib `scripts/test-*.sh` 的模式一致。

1. **RAG 引用**：问"pgstore 怎么建表"，回答的来源含 `goagent-contrib:durable/pgstore/`。
2. **记忆写入与召回**：u1 说"我们用 pgstore"；在新会话里问存储选型，注入的提示词中含这条记忆。u2 的会话里没有。
3. **跨实例一致**：
   - 经实例 a 的接口写入记忆，b 的召回能看到。
   - 经 b 的接口删除这条记忆后，a 的召回不再包含它。
   - 删除前用这条记忆生成的模型缓存条目，在 Redis 中已被物理删除。
4. **审批与恢复**：
   - u1 请求清空记忆，任务停在待审批。
   - 杀掉 a，执行 failover 流程，在 b 上批准。
   - 清空完成，且 `Purge` 只执行一次（用调用计数断言）。
5. **拒绝审批**：拒绝后记忆仍在，任务正常结束，结果写明"用户拒绝，未清空"。
6. **重建索引**：批准后建新 collection 并切换 alias；切换前后的问答都有来源。

**真实模型（`scripts/live.sh`，需显式开启）**：用真实对话模型与 Gemini 嵌入跑固定问答，覆盖场景 1–3。

- **失败条件**（任一出现，脚本以非零退出）：
  - 有问题的来源没有命中期望的路径前缀；
  - 记忆没有召回；
  - 删除后仍能召回。
- **报告**：JSON 格式，存到 `verification/`，含每题的来源、召回内容、token 用量、耗时。

## 7. 产出

- `products/knowledge-assistant/`：代码、`docker-compose.yml`、`README.md`（搭建指南）、`FINDINGS.md`（框架问题与已知限制）、`verification/`（真实模型报告）。
- 仓库根 README 目录表加一行。

## 8. 留给实现者

- 切分大小、重叠、召回条数、是否重排：按效果自定，在 README 说明取值理由。
- Agent 的系统提示词与工具命名。
- 确定性嵌入器的具体实现。
- 记忆抽取同步还是异步。
- 页面布局。
