# 客服编排接入长期记忆设计

日期：2026-11-17

## 意图

在 `orchestration/customer-service` 中接入 goagent 的长期记忆（`memory` 包），用真实模型和本地 docker 存储端到端验证记忆功能，重点是 goagent master 上刚修复的两处：

- 整理时取代钉住记录的新记录继承钉住（47e1f5f5）；
- 同一轮里否定计划记录时不再空指针 panic（49dd0983）。

验证用本地代码，不发布版本：发布的版本必须是稳定的，验证中发现问题时不应因此再发一次版。

## 目标

1. 客户跨工单的事实与偏好被记住，后续工单的成员 Agent 能召回。
2. 客户明确要求记住的内容存为钉住记忆，之后被整理合并或否定时，取代者仍是钉住的。
3. 用真实模型跑通：对话用 `internal/livemodel`（MiniMax / DeepSeek），嵌入用 Gemini。
4. 存储全部来自本地 docker：Postgres 存记忆数据与会话日志，Qdrant 存记忆向量索引。
5. 离线（demomodel）有一条确定性回归，覆盖同轮否定计划记录的路径。

## 非目标

- 不发布 goagent、goagent-contrib 的任何版本，不打 tag。
- 不改变工单流程本身：DurableGraph 的检查点仍在 `-dir` 的 FileStore，退款账本仍是 SQLite。
- 不做记忆管理界面；只加一个查看记忆的命令。
- 分流 Agent 不接记忆，路由仍只看本工单。

## 反目标

- 记忆命名空间取自模型输出或客户原文（必须取自宿主可信的 CustomerID）。
- 未开启记忆时，现有离线测试与行为发生任何变化。
- 把 API key 写入文件、日志、提交或报告。

## 约束

- goagent 以本地 master 为准（包含上述两个修复），通过 `go.work` 使用本地代码。
- 新的 Qdrant 记忆索引是 goagent-contrib 的新模块 `memory/qdrant`，在基于 contrib master 的独立 worktree 中开发；contrib 主 worktree（当前在用户的 `feat/pgstore-snapshot-lz4` 分支）不动。
- 因为 `memory/qdrant` 没有发布版本，customer-service 只能在工作区模式下编译；`make test-remote`（`GOWORK=off`）在发布之前会失败。这是"不发版"的直接后果，在 README 中写明。
- docker 容器为专用、用后删除，做法同 knowledge-assistant 的 `scripts/test.sh`；端口与已在运行的 knowledge-assistant 栈不冲突。
- 测试运行：`TMPDIR=/tmp taskset -c 2,3,6-27`。

## 方案

考虑过的存储方案：

| 方案 | 内容 | 取舍 |
|---|---|---|
| A | 单个 pgvector 容器：`durable/pgstore` + `memory/pgvector` | 现成组件，最省事；不覆盖新的索引后端 |
| **B（选定）** | Postgres 存记忆数据与会话日志，Qdrant 做记忆向量索引 | 多一个容器，需新写 Qdrant 版 `memory.Index`；同时验证一个新的索引后端 |

用户选择 B。Qdrant 记忆索引的位置考虑过放在示例内部，用户选择做成 contrib 新模块：可复用，可用 `memorytest.RunIndex` 单独验证契约。

## 设计

### 1. goagent-contrib：`memory/qdrant`

- 新模块 `github.com/kart-io/goagent-contrib/memory/qdrant`，形状照 `memory/pgvector`（`New(ctx, Config) (*Index, error)`，实现 `memory.Index`）。
- 必须通过 `memorytest.RunIndex` 全部用例：命名空间精确隔离（不含子命名空间）、归一化、拒绝零向量与非有限值、同一命名空间混合维度且 Search 只看同维度、结果顺序与平分、limit、覆盖写、`IDs`、`Clear` 不递归。
- 命名空间与维度到 Qdrant 集合/点的映射、点 ID 的构造由实现者决定，但须满足上面的契约（Qdrant 点 ID 只接受 UUID 或无符号整数，需由命名空间与键确定性地生成，并在 payload 中保留原键）。
- 集成测试在没有 Qdrant 地址的环境变量时跳过，变量名与 contrib 现有约定一致。
- 依赖用 `github.com/qdrant/go-client`（contrib `retrieval/qdrant` 已在用）。

### 2. customer-service 装配

开关：新增 `-memory` 选项（`message`、`answer`、`sweep`、`memory` 命令可用）。未给出时行为与现在完全相同。

开启时需要：
- `-pg` Postgres 连接串、`-qdrant` Qdrant 地址（也可取环境变量，名称由实现者定，写进 README）。
- 记忆数据：contrib `durable/pgstore.Store`；会话日志：`pgstore.SessionStore`；索引：`memory/qdrant`。
- 模型：`-live` 时抽取与整理用同一个 `livemodel` 客户端，嵌入用 goagent `llm/providers` 的 Gemini 原生客户端（`GEMINI_API_KEY`）；离线时用 demomodel 和一个确定性的本地嵌入（如按字符哈希的固定维度向量）。
- `EmbedModel` 标识随嵌入来源变化（离线与真实不同），避免同一目录混用。

接入范围：
- 退款成员与通用咨询成员：`WithSessionLog` + `WithLongTermMemory`。会话 ID 由宿主按"工单 ID/成员"给出，保证每个成员在一张工单里是一个会话。
- 通用咨询成员额外注册 `memory_save`、`memory_forget`（来自 `mem.Tools()`），客户明确说"记住……"时由它保存为钉住记忆。提示词补一句何时使用。
- 分流 Agent 不接记忆。
- 命名空间：宿主在调用图之前 `memory.WithNamespace(ctx, "customer-service", CustomerID)`，CustomerID 来自工单（宿主已认证的输入），不来自模型。

Worker：
- CLI 每条命令都是新进程，不常驻 Worker。`sweep` 在现有扫描之后执行一次 `ProcessPending`（其中先做 `Maintain`），把本次之前登记的抽取任务处理完。
- 失败任务按 `JobAttempts` 重试；`sweep` 输出处理的任务数与失败数。

查看命令：`customer-service memory -dir D -memory -customer C [-q 文本]`，输出钉住记忆与按 q 召回的记录（含 `Pinned`、`SupersededBy`、`InvalidAt`），供验证时判断继承是否成立。

### 3. 本地容器

新脚本（位置在 `orchestration/customer-service/scripts/`）启动 Postgres（`pgvector/pgvector:pg17` 或官方 postgres 镜像均可）与 `qdrant/qdrant:v1.20.2` 专用容器，等就绪后导出连接变量，执行传入的命令，退出时删除容器。默认端口避开 knowledge-assistant 栈与 ka 测试脚本使用的端口。

### 4. 验证

离线（默认测试，开启容器时运行，否则跳过）：
1. 开启记忆后的完整流程：两张工单，同一客户；`sweep` 后第二张工单的成员请求中含召回块。
2. 钉住继承：demomodel 让通用成员调用 `memory_save` 钉住一条；后续工单的抽取与整理脚本输出 `update` 指向它；`sweep` 后新记录 `Pinned` 为真、旧记录 `SupersededBy` 指向新记录。
3. 同轮否定计划记录：抽取脚本在同一任务里输出一条新事实与一条否定它的候选，整理脚本输出 `add` 再 `invalidate` 指向前者；`sweep` 不 panic，任务状态为完成，结果与 goagent 单元测试 `TestCommitJobChecksAgainstStateBeforeTask` 的对应用例一致。
4. 未开启 `-memory` 时现有测试全部通过且不需要容器。

真实模型（`GOAGENT_LIVE=1` 且容器可用时运行，沿用 `live_test.go` 的门控方式）：
1. 工单 1：客户说"退款请只退回原支付方式"；`sweep`；工单 2 退款时成员请求含该事实。
2. 客户说"请记住：只用邮件联系我"；确认存为钉住；工单 3 客户说"邮件或短信都可以"；`sweep` 后查看命令显示新内容仍钉住。
3. 真实模型下断言只针对可观察的不变量（不 panic、任务完成、钉住不丢失），不断言模型措辞。

goagent-contrib：`memory/qdrant` 用 `memorytest.RunIndex` 对真实 Qdrant 容器运行。

### 5. 工作区与构建

- examples：worktree `goagent-examples-cs-mem`（分支 `feat/cs-memory`，基于 main）。
- contrib：新 worktree，基于 contrib master。
- 构建与测试用临时 `go.work`（`GOWORK=<文件>`），use：examples worktree、`../goagent`、contrib worktree 的 `durable/pgstore` 与 `memory/qdrant`（以及 examples 现有的 `acp`）。生成方式写进 README 或 Makefile 的 `work` 目标。
- 用户在 examples 主 worktree 中未提交的 codec/json 迁移不受影响；之后合并时 `main.go`、`host.go` 可能有冲突，届时处理。

## 已定的实现细节（设计者的决定，可推翻）

- **离线脚本引用记录 ID**：记录 ID 由任务 Key 与序号确定（memory README"抽取流程"第 5 条），离线脚本按工单与成员可预先算出，或测试先用查看接口取到 ID 再生成后续工单的整理脚本。demomodel 按请求的系统提示区分对话、抽取、整理三类调用；现有 demomodel 不支持时在示例内扩展，不改 goagent。
- **sweep 的统计**：输出 `ProcessPending` 的取出数与 `DroppedJobs`；测试的"任务完成"用 `mem.Job(ctx, key)` 的状态断言，不靠 sweep 输出。
- **命名空间贯穿所有入口**：`message`、`answer`、`sweep`（续跑超时工单）都从账本的工单行读取 CustomerID 再绑定命名空间，任何路径都不从请求或模型取。会话 ID 的分隔符由实现者选，须被 `pgstore.SessionStore` 接受（先查其校验规则）。
- **中断恢复的覆盖**：用已有的退款追问路径（`ask_customer` → 进程退出 → `answer` 新进程续跑）作为跨进程恢复用例，不新增崩溃注入设施。
- **测试隔离**：每个测试用唯一 CustomerID（命名空间即隔离）；`memory/qdrant` 契约测试每个用例用唯一集合前缀；测试包串行（`-p 1`），同 knowledge-assistant。
- **未发布模块的依赖**：examples 的 `go.mod` 不为 `memory/qdrant` 写占位版本，依赖工作区解析；`go mod tidy` 不在本次运行。发布 contrib 后再补 `require`。
- **真实嵌入**：Gemini `gemini-embedding-001`（与 knowledge-assistant 相同，`GEMINI_EMBEDDING_MODEL` 可覆盖），`EmbedModel` 取"模型名/维度"，离线哈希嵌入取不同的标识。

## 留给实现者

- Qdrant 集合与点 ID 的具体映射、批量与超时设置。
- 选项与环境变量的具体名称、查看命令的输出格式。
- 离线确定性嵌入的实现方式与维度。
- 容器脚本的端口默认值。

## 风险

- 会话日志在 Postgres、工单检查点在 FileStore，两者分属不同存储；进程在一轮中途崩溃后恢复的行为依赖 builder 的"同一执行键续写该轮"，需在离线测试中覆盖一次中断恢复（如已有的追问续跑路径）。
- 真实模型输出不稳定，整理不一定给出 `update`/`invalidate`；真实模型验证只保证不变量，确定性路径由离线脚本覆盖。
- `GOWORK=off` 构建在 contrib 发布前失败。
