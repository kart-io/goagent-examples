# 客服编排接入长期记忆 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 customer-service 接入 goagent 长期记忆（Postgres 存数据与会话、Qdrant 做向量索引），用离线脚本与真实模型验证钉住继承与同轮否定两处修复。

**Architecture:** contrib 新模块 `memory/qdrant` 实现 `memory.Index`；customer-service 新增 `memstore.go` 装配记忆，宿主在 `step` 绑定命名空间与会话，退款与通用成员接记忆，`sweep` 处理抽取任务，新增 `memory` 查看命令。全部依赖经 `go.work` 使用本地 goagent master 与 contrib worktree。

**Tech Stack:** Go 1.27、goagent（本地 master）、goagent-contrib `durable/pgstore` 与新 `memory/qdrant`、`github.com/qdrant/go-client`、pgx/v5、docker（`pgvector/pgvector:pg17`、`qdrant/qdrant:v1.20.2`）。

**Spec:** `docs/superpowers/specs/2026-11-17-customer-service-memory-design.md`

## Global Constraints

- 不发布任何版本、不打 tag；examples 的 `go.mod` 不新增 contrib 的 require，靠 `go.work` 解析。
- 两个 worktree：examples `../goagent-examples-cs-mem`（分支 `feat/cs-memory`）；contrib `../goagent-contrib-mem-qdrant`（分支 `feat/memory-qdrant`，基于 contrib `master`）。contrib 主 worktree 与 examples 主 worktree 不碰。
- 命名空间固定为 `[]string{"customer-service", <工单行的 CustomerID>}`，只在宿主 `step` 中绑定。
- 未给 `-memory` 时行为、输出与现有测试完全不变，且不需要容器。
- 环境变量：`CS_MEMORY_PG_URL`、`CS_MEMORY_QDRANT_ADDR`（gRPC 地址 host:port）；contrib 契约测试用 `GOAGENT_QDRANT_ADDR`。缺少时相关测试 `t.Skipf`。
- 容器端口默认：Postgres `55450`，Qdrant gRPC `56344`、HTTP `56343`（避开 ka 栈与 ka 测试脚本的 55440/56390/56334/56333）。
- 真实嵌入：`providers.NewGeminiWithOptions()`（`GEMINI_API_KEY`、`GEMINI_EMBEDDING_MODEL` 默认 `gemini-embedding-001`），`EmbedModel` 为 `"gemini/<模型名>/<维度>"`；离线哈希嵌入 `EmbedModel` 为 `"hash/<维度>"`。
- 注释、错误信息、提交信息用简体中文；不写 API key 到任何文件。
- 测试命令前缀：`TMPDIR=/tmp taskset -c 2,3,6-27`；包间串行 `-p 1`。

## Review Focus

1. 同一客户的第二张工单在不同进程中打开（重开宿主）时召回仍可用：Task 4 的跨工单测试在两张工单之间重开宿主。
2. 退款追问跨进程续跑（`message` 回答追问）时命名空间与会话不变，不报 `ErrMemoryNamespaceChanged`：Task 4 加追问续跑用例。
3. 不同客户互不可见：Task 4 断言客户 B 的召回与 `Pinned` 为空。
4. Qdrant 同一 ID 换维度写入后旧维度集合里不残留：契约用例"同ID换维度覆盖"已覆盖，Task 1 不得跳过。
5. `sweep` 在记忆任务失败时仍完成工单扫描与发件箱投递：Task 4 用一个返回非法 JSON 的抽取脚本断言 sweep 的工单部分照常、错误被汇总返回。

---

### Task 1: contrib `memory/qdrant` 模块

**Files（均在 `../goagent-contrib-mem-qdrant`）：**
- Create: `memory/qdrant/go.mod`、`memory/qdrant/qdrant.go`、`memory/qdrant/qdrant_test.go`、`memory/qdrant/README.md`

**Interfaces:**
- Produces:
  - `type Config struct { Client *qdrant.Client; Prefix string }`（`Prefix` 为集合名前缀，默认 `"goagent_memory"`）
  - `func New(ctx context.Context, cfg Config) (*Index, error)`；`var _ memory.Index = (*Index)(nil)`

- [ ] **Step 0: 建 worktree**：`git -C ../goagent-contrib worktree add -b feat/memory-qdrant ../goagent-contrib-mem-qdrant master`
- [ ] **Step 1: 写失败测试** `TestIndexContract`：读 `GOAGENT_QDRANT_ADDR`（缺少则 Skip），每个子用例用唯一前缀（如 `"t" + 随机十六进制`）新建 Index，`t.Cleanup` 删除该前缀的全部集合；调用 `memorytest.RunIndex(t, newIndex)`。另加 `TestNewRequiresClient`：`New(ctx, Config{})` 返回 `errors.CodeInvalidInput`。
- [ ] **Step 2: 起 Qdrant 容器运行，确认失败**：`docker run -d --rm --name qd-mem -p 127.0.0.1:56344:6334 qdrant/qdrant:v1.20.2`；`GOAGENT_QDRANT_ADDR=127.0.0.1:56344 go test ./memory/qdrant/`（用临时 go.work 指向本地 goagent）。预期：编译失败。
- [ ] **Step 3: 实现。** 映射规则（契约决定不了的部分）：
  - 每个维度一个集合 `<Prefix>_d<维度>`，距离 Cosine，首次写入时创建（并发创建遇"已存在"视为成功）。
  - 点 ID：`uuid.NewSHA1(固定命名空间UUID, nsKey + "\x00" + id)`，`nsKey = strings.Join(ns, "\x00")`；payload 存 `ns`（nsKey，建 keyword 索引）与 `id`。
  - `Upsert`：`memory.ValidateNamespace`、`memory.NormalizeVector` 后写入本维度集合，再从其他维度集合删除同一点 ID（"同ID换维度覆盖"）。
  - `Search`：k≤0 返回 nil；只查与查询同维度的集合，按 `ns` 精确过滤，`exact: true`。并列规则与 `MemoryIndex` 一致（分数降序，并列按 ID 字节序升序）：先取 k 条；若第 k 条与其后可能存在并列，则以第 k 条分数减 1e-6 为 `score_threshold` 取出全部候选，本地按（分数降序、ID 升序）排序后截断到 k。集合不存在返回 nil。
  - `IDs` / `Clear` / `Delete`：遍历本前缀的全部集合，按 `ns` 精确过滤（scroll / 按过滤删除 / 按点 ID 删除）。
  - `go.mod` require 的 goagent 版本与 `memory/pgvector` 相同（v0.54.0），依赖 `github.com/qdrant/go-client` 与 `github.com/google/uuid`。
  - README 写前置条件、映射规则与运行测试的命令，形状照 `memory/pgvector`。
- [ ] **Step 4: 运行通过**：同 Step 2 命令，预期 `ok`，12 个契约子用例全部 PASS、无 SKIP；再跑 `go vet ./memory/qdrant/`。
- [ ] **Step 5: 提交**：`git add memory/qdrant && git commit -m "feat(memory/qdrant): 新增基于 Qdrant 的长期记忆向量索引"`；删除容器 `docker rm -f qd-mem`。

### Task 2: examples 工作区、容器脚本与记忆装配

**Files:**
- Modify: `Makefile`（`work` 目标）
- Create: `orchestration/customer-service/scripts/with-stores.sh`
- Create: `orchestration/customer-service/memstore.go`、`orchestration/customer-service/memstore_test.go`

**Interfaces:**
- Consumes: Task 1 `qdrant.New`、`qdrant.Config`
- Produces:
  - `type MemoryConfig struct { PGURL, QdrantAddr string; Live bool }`
  - `type memoryStores struct { Memory *memory.Memory; Sessions session.Store; close func() }`，`func (s *memoryStores) Close()`
  - `func openMemory(ctx context.Context, c MemoryConfig, model llm.Client) (*memoryStores, error)`：pgxpool → `pgstore.New` + 执行 `pgstore.Schema + pgstore.SessionSchema`（幂等，参考 ka `assistant.Migrate`）→ `pgstore.NewSessionStore` → qdrant 客户端与 `qdrant.New(Prefix:"cs_memory")` → 嵌入（`Live` 时 Gemini，否则 `hashEmbedder{dims: 256}`）→ `memory.New(Config{Durable, Index, Embedder, EmbedModel, Model: model, Sessions})`。
  - `type hashEmbedder struct{ dims int }`，`Embed` 按字符二元组哈希累加到固定维度，确定性、非零。
  - 命名空间常量 `memoryApp = "customer-service"`，`func memoryNS(customerID string) []string`

- [ ] **Step 1: `make work` 支持 contrib**：`CONTRIB ?=` 为空时行为不变；非空时追加 `go work use $(CONTRIB)/durable/pgstore $(CONTRIB)/memory/qdrant`，并把 `-go` 设为 `1.27.0`（与 goagent master 一致）。运行 `make work CONTRIB=../goagent-contrib-mem-qdrant`，`go build ./orchestration/customer-service/` 通过。
- [ ] **Step 2: 容器脚本**：照 ka `scripts/test.sh`：启动 `cs-mem-pg-$$`（`pgvector/pgvector:pg17`）与 `cs-mem-qdrant-$$`，等待就绪，导出 `CS_MEMORY_PG_URL`、`CS_MEMORY_QDRANT_ADDR`，执行 `"$@"`，`trap` 删除容器。用法注释写在脚本头。
- [ ] **Step 3: 写失败测试** `TestOpenMemory`（缺环境变量 Skip）：用唯一 CustomerID 绑定命名空间；`Remember` 一条 `Pinned: true` 的 fact，`Pinned` 返回它；`Recall(Query{Text: 同内容, K: 5})` 命中；关闭后重新 `openMemory`，`Pinned` 仍返回它（跨进程）。另加 `TestHashEmbedder`：同文本向量相等、不同文本不等、长度 256、非零。
- [ ] **Step 4: 运行确认失败**：`scripts/with-stores.sh go test -run 'TestOpenMemory|TestHashEmbedder' ./orchestration/customer-service/`
- [ ] **Step 5: 实现 `memstore.go`**
- [ ] **Step 6: 运行通过**（同 Step 4），并确认不经脚本直接 `go test ./orchestration/customer-service/` 时 `TestOpenMemory` 为 SKIP、其余全过。
- [ ] **Step 7: 提交**：`git add Makefile orchestration/customer-service/scripts/with-stores.sh orchestration/customer-service/memstore.go orchestration/customer-service/memstore_test.go && git commit -m "feat(customer-service): 装配 Postgres 与 Qdrant 支撑的长期记忆"`

### Task 3: 离线脚本支持记忆调用

**Files:**
- Modify: `orchestration/customer-service/model.go`、`orchestration/customer-service/model_test.go`

**Interfaces:**
- Produces（脚本约定，Task 4 测试依赖）：
  - 通用成员：客户原文以"请记住："开头时，先调用 `memory_save`（`{"content": 冒号后的原文, "kind":"fact", "pinned": true}`），工具结果返回后回复"好的，已记住。"；否则保持现有回复。
  - 抽取（系统提示以 `你从一轮对话中抽取` 开头）：对话中出现"只退回原支付方式"→ 一条 fact `客户要求退款只退回原支付方式`；出现"邮件或短信都可以"→ 一条 fact `客户接受邮件或短信联系`；出现"先说住杭州又改口"→ 两条 fact：`客户住在杭州`、`客户不住在杭州`；出现"抽取坏回复"→ 返回非 JSON 文本；其余 `{"memories":[]}`。
  - 整理（系统提示以 `你维护长期记忆` 开头）：从用户消息解析 `- id=<ID>：<内容>` 行。新信息含"邮件或短信"且有内容含"邮件"的候选 → `{"op":"update","target":该ID,"content":"客户接受邮件或短信联系"}`；新信息为 `客户不住在杭州` 且有内容为 `客户住在杭州` 的候选 → `{"op":"invalidate","target":该ID}`；否则 `{"op":"add"}`。

- [ ] **Step 1: 写失败测试**（表驱动 `TestOfflineMemoryScripts`）：直接调用 `respond` 构造请求，覆盖上面每条约定（含 invalidate 解析出正确 target、无候选时 add）。
- [ ] **Step 2: 运行确认失败**：`go test -run TestOfflineMemoryScripts ./orchestration/customer-service/`
- [ ] **Step 3: 实现**：`respond` 先按系统提示前缀分派到 `extractTurn`、`reconcileTurn`；通用成员按 `req.Messages` 中是否已有 `memory_save` 的工具结果决定调用工具或回复。
- [ ] **Step 4: 运行通过**，并跑全包确认现有离线测试不变。
- [ ] **Step 5: 提交**：`git commit -m "test(customer-service): 离线脚本覆盖记忆抽取、整理与 memory_save"`（只 add `model.go`、`model_test.go`）

### Task 4: 宿主接入记忆

**Files:**
- Modify: `orchestration/customer-service/host.go`（`HostConfig`、`OpenHost`、`step`、`Close`）、`graph.go`（`agentFactory`、`newGraph`）、`refund.go`（`refundMember` 传入记忆选项）、`sweeper.go`（`Sweep`、`SweepReport`）
- Create: `orchestration/customer-service/memory_test.go`

**Interfaces:**
- Consumes: Task 2 `openMemory`、`memoryStores`、`memoryNS`；Task 3 脚本约定
- Produces:
  - `HostConfig.Memory *MemoryConfig`（nil 表示关闭）；`Host.memory *memoryStores`
  - `type memberMemory struct { mem *memory.Memory; sessions session.Store; tools bool }`；`agentFactory(client, scope, prompt, iterations, newTools, mm *memberMemory)`，`mm` 为 nil 时与现在完全相同；非 nil 时 `WithSessionLog(sessions).WithLongTermMemory(mem)`，会话 ID 为 `<工单ID>:<scope>`（工单 ID 由 `step` 经 context 传入，参照 `withCustomer`；先确认 `pgstore.SessionStore` 接受 `:`），`tools` 为真时追加 `mem.Tools()`。
  - 通用成员 `tools: true`，退款成员 `tools: false`，分流 `nil`。通用成员提示词补一句：客户明确要求记住时调用 `memory_save` 并设 `pinned` 为 true。
  - `step` 中 `runCtx = memory.WithNamespace(runCtx, memoryNS(row.CustomerID)...)`（签名以 goagent 为准）与工单 ID 绑定，仅在记忆开启时。
  - `SweepReport` 新增 `MemoryJobs int \`json:"memory_jobs,omitempty"\``、`MemoryDropped int64 \`json:"memory_dropped,omitempty"\``；`Sweep` 在投递发件箱之后调用 `ProcessPending`，错误并入 `errors.Join`。

- [ ] **Step 1: 写失败测试**（`memory_test.go`，全部缺环境变量时 Skip，每个测试唯一 CustomerID）：
  - `TestMemoryAcrossTickets`：客户 c 发"订单 A1001 申请退款 30 元，只退回原支付方式"→ `Sweep` → 重开宿主 → 客户 c 再发一条通用咨询；断言第二次通用成员收到的请求中有以 `memory.RecallPrefix` 开头且含 `只退回原支付方式` 的用户消息（离线模型记录请求）；客户 d 的 `Recall` 与 `Pinned` 为空。
  - `TestPinnedSurvivesUpdate`：客户 c 发"请记住：只用邮件联系我"→ `Pinned` 有 1 条 → 新工单发"邮件或短信都可以" → `Sweep` → `Pinned` 恰 1 条，内容为 `客户接受邮件或短信联系`；用 `Recall` 或 Durable 读到旧记录的 `SupersededBy` 等于新记录 ID。
  - `TestSameTurnNegateNoPanic`：客户 c 发"先说住杭州又改口" → `Sweep` 返回 nil 错误；`Recall("杭州")` 结果中无 `客户住在杭州` 的有效记录，有 `客户不住在杭州`；`SweepReport.MemoryJobs ≥ 1`。
  - `TestFollowUpKeepsNamespace`：触发退款追问（缺订单号）→ 重开宿主 → `message` 回答追问 → 无错误、工单推进。
  - `TestSweepMemoryFailure`：客户发"抽取坏回复" → `Sweep` 返回的错误非 nil（任务失败重试耗尽后），但同一次 `Sweep` 的发件箱已投递（`notifier` 收到回复）。
  - `TestNoMemoryUnchanged`：`HostConfig.Memory` 为 nil 时现有流程不建任何 PG/Qdrant 连接（不设环境变量也能跑）。
- [ ] **Step 2: 运行确认失败**：`scripts/with-stores.sh go test -p 1 -run 'TestMemory|TestPinned|TestSameTurn|TestFollowUp|TestSweepMemory|TestNoMemory' ./orchestration/customer-service/`
- [ ] **Step 3: 实现上述接口**；`OpenHost` 记忆开启时 `openMemory`，失败要关闭已开的账本；`Close` 一并关闭。
- [ ] **Step 4: 运行通过**（同 Step 2），再经脚本跑全包 `-race`，最后不经脚本跑全包（记忆测试 SKIP，其余 PASS）。
- [ ] **Step 5: 提交**：只 add 上列文件，`git commit -m "feat(customer-service): 退款与通用成员接入长期记忆，sweep 处理抽取任务"`

### Task 5: CLI 与查看命令

**Files:**
- Modify: `orchestration/customer-service/main.go`、`cli_test.go`

**Interfaces:**
- Consumes: Task 4 `HostConfig.Memory`
- Produces:
  - 选项 `-memory`（bool）、`-pg`（默认 `$CS_MEMORY_PG_URL`）、`-qdrant`（默认 `$CS_MEMORY_QDRANT_ADDR`）、`-q`（召回文本）；`-memory` 且缺 `-pg`/`-qdrant` 时报错"-memory 需要 -pg 与 -qdrant（或环境变量 CS_MEMORY_PG_URL、CS_MEMORY_QDRANT_ADDR）"。`Live` 取 `-live`。
  - 命令 `memory -customer C [-q 文本]`（需要 `-memory`，`InspectOnly` 为真）：输出 `{"pinned":[...],"matches":[...]}`，空时为 `[]`；记录含 `id`、`content`、`pinned`、`superseded_by`、`invalid_at`（用 `memory.Record` 原样编码即可，字段名以其 JSON 标签为准）。
  - `usage` 增加 `memory` 行与 `-memory` 说明。

- [ ] **Step 1: 写失败测试**：`TestCLIMemoryRequiresStores`（`-memory` 无地址时报上述错误，表驱动覆盖缺 pg、缺 qdrant）；`TestCLIMemoryShow`（经脚本：`message` "请记住：只用邮件联系我" 后 `memory -customer` 输出 pinned 长度 1，`matches` 为 `[]`）。
- [ ] **Step 2: 运行确认失败** → **Step 3: 实现** → **Step 4: 运行通过**（经脚本与不经脚本各一次）。
- [ ] **Step 5: 提交**：`git commit -m "feat(customer-service): 增加 -memory 选项与 memory 查看命令"`

### Task 6: 真实模型验证与文档

**Files:**
- Modify: `orchestration/customer-service/live_test.go`、`orchestration/customer-service/README.md`

- [ ] **Step 1: 写 `TestLiveMemory`**：`livemodeltest.Require(t)` 门控，另要求 `GEMINI_API_KEY` 与两个存储环境变量（缺则 Skip）。`HostConfig{Client: client, Memory: &MemoryConfig{Live: true, ...}}`，唯一 CustomerID。步骤：发"订单 A1001 申请退款 30 元，退款请只退回原支付方式"→ Sweep → 发"请记住：只用邮件联系我"→ 断言 `Pinned` ≥1 → 发"以后邮件或短信都可以联系我"→ Sweep → 断言：Sweep 无错误、所有任务状态非 failed、`Pinned` 非空且至少一条内容含"短信"或仍为原钉住记录（模型可能 noop）。`t.Log("LIVE_EVIDENCE " + ...)` 输出 Pinned 内容、SupersededBy、任务统计，不输出密钥。
- [ ] **Step 2: 运行**：`GOAGENT_LIVE=1 scripts/with-stores.sh go test -p 1 -run TestLiveMemory -v ./orchestration/customer-service/`，预期 PASS，记录 LIVE_EVIDENCE。
- [ ] **Step 3: README**：新增"长期记忆"一节：存储与容器脚本、`make work CONTRIB=...`、选项、`memory` 命令、离线与真实验证命令；写明 contrib `memory/qdrant` 未发布前只能在工作区模式编译，`make test-remote` 会失败。依赖说明里的 goagent 版本描述更正为"本地 master（未发布）"。
- [ ] **Step 4: 全量验证**：经脚本跑 `go vet ./orchestration/... && go test -p 1 -race ./orchestration/...`；不经脚本再跑一次。
- [ ] **Step 5: 提交**：`git commit -m "test(customer-service): 真实模型验证长期记忆，补充 README"`
