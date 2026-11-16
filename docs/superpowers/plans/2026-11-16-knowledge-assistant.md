# 团队知识助手 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal：** 在 `products/knowledge-assistant` 交付一个可部署的团队知识助手。它提供：
- 文档问答（RAG，附来源）；
- 按用户的长期记忆；
- 经审批的清空记忆与重建索引；
- 双实例部署与故障转移。

同时把组装中暴露的框架问题写进 `FINDINGS.md`。

**Architecture：**
- 一个二进制 `ka`，子命令为 `migrate`、`ingest`、`serve`、`recover-worker`。
- `serve` 挂核心 `management/httpapi`、产品 REST 接口和静态页面。设置 `WORKER=1` 时同时运行 `management` Worker。
- Agent 由 `adapters.NativeFactory` 组装，外面包一层 Factory，按 `Task.TenantID` 绑定记忆命名空间。
- 状态全部放在共享存储：Postgres（management、durable、session、记忆、pgvector）、Redis（模型缓存）、Qdrant（文档向量，经 alias 读）。

**Tech Stack：**
- Go 1.27，goagent v0.54.1。
- contrib 模块：`durable/pgstore`、`management/pgstore`、`memory/pgvector`、`cache/redis`、`document`、`retrieval`、`retrieval/qdrant`。
- `github.com/qdrant/go-client`、pgx/v5、go-redis/v9。
- Docker Compose。

**Spec：** `docs/superpowers/specs/2026-11-16-knowledge-assistant-design.md`（用户 2026-11-16 批准）。

## Global Constraints

- 位置：`products/knowledge-assistant`，独立 go module `github.com/kart-io/goagent-examples/products/knowledge-assistant`，`go 1.27.0`。
- goagent 用 `v0.54.1`。
- contrib 版本：
  - `durable/pgstore/v0.5.0`、`cache/redis/v0.1.0`、`memory/pgvector/v0.1.1`。
  - `document/v0.3.1`、`retrieval/v0.4.1`、`retrieval/qdrant/v0.4.1`，在 Task 1 发版。
  - `management` 用 pseudo-version `@396d3eca4884`，该模块无 tag。
- 只用已推送的提交，go.mod 不写 `replace`，独立克隆即可构建。
- 反目标：
  - 不在产品里自写审批状态机、任务恢复、缓存擦除，也不直接删 Redis 键。
  - 遇到框架缺口，在 `FINDINGS.md` 记录。
- 回答必须带来源，格式为 `仓库名:仓库相对路径#小节`，例如 `goagent-contrib:durable/pgstore/README.md#建表`。
- 用户身份：`Authorization: Bearer <用户名>`。这个用户名同时作为：
  - `Actor.TenantID` 和 `Actor.Subject`；
  - 记忆命名空间 `[]string{用户名}`。

  README 写明这不是认证。
- 默认嵌入用 Gemini `gemini-embedding-001`，维度在启动时探测，不写死。离线测试用 3072 维哈希嵌入器。
- 只有设置 `WORKER=1` 的实例运行 Worker。故障转移经 `ConfirmWorkerStopped` + `RecoverWorkerSlot`。
- 注释、错误信息、文档、提交信息用简体中文。
- 测试命令带 `taskset -c 2,3,6-27`、`TMPDIR=/tmp`，禁用 `-mod=mod`。
- 不改本仓库已有的未提交文件；提交只 add 明确路径。
  - 例外：根 `README.md` 当前干净，在 Task 9 加一行。
  - 拷贝 livemodel 时取 `git show HEAD:internal/livemodel/...`，不取工作区里的 `minimax.go` 修改。
- 推送 tag 或分支前必须征得用户同意。

## Review Focus

1. **审批期间杀掉持有 Worker 的实例。**
   - 期望：b 接管后，Purge 恰好执行一次；审批记录仍是 pending，在 b 上批准后任务完成。
   - 测试：Task 8 `TestApprovalSurvivesKillAndFailover`。
2. **同一用户两条并发对话共享一个命名空间，另一用户的记忆和缓存不可见。**
   - 期望：u2 召回不到 u1 的记忆；u2 不命中 u1 的缓存条目。
   - 测试：Task 8 `TestMemoryIsolatedPerUser`，其中断言 u2 的回显不含 u1 的记忆文本。
3. **Qdrant alias 不存在时启动 `serve`（未跑 ingest）。**
   - 期望：启动失败并提示先运行 ingest，而不是隐式建出一个与 alias 同名的空集合。
   - 测试：Task 4 `TestRetrieverRequiresAlias`。
4. **重建索引的工具在 alias 切换后、删除旧集合前崩溃，然后被重放。**
   - 期望：重放不新建第二个集合，alias 指向同一个新集合，旧集合最终被删除。
   - 测试：Task 4 `TestRebuildIsIdempotent`。
5. **Bearer 头缺失、为空或含非法字符**（空白、`/`、超过 64 字节）。
   - 期望：返回 401，不会落到空租户或共享租户。
   - 测试：Task 7 `TestActorFromRequest`。

---

## 文件结构

```
products/knowledge-assistant/
  go.mod, go.sum
  cmd/ka/main.go              子命令分派与进程装配（serve/ingest/migrate/recover-worker）
  internal/config/config.go   环境变量 → Config
  internal/embed/gemini.go    Gemini → retrieval.Embedder 适配（探测维度）
  internal/embed/hash.go      确定性哈希嵌入器（离线测试）
  internal/docs/collect.go    Markdown 按小节 + go/doc 包文档 → []Chunk
  internal/docs/index.go      Qdrant：Rebuild（新集合 + alias 切换）、Retriever（只读 alias）
  internal/chat/livemodel.go  拷贝自 examples internal/livemodel（HEAD 版本）
  internal/chat/minimax.go    同上
  internal/chat/script.go     确定性脚本模型（离线测试）
  internal/assistant/stores.go   Postgres/Redis 装配与迁移
  internal/assistant/factory.go  NativeFactory + 命名空间包装 + 工具与审批钩子
  internal/assistant/agent.go    按用户幂等注册/发布 Agent
  internal/web/server.go         ActorFromRequest、产品 REST、静态页面、httpapi 挂载
  internal/web/static/{index.html,app.js,style.css}
  e2e/e2e_test.go             以子进程启动 a/b 两个实例跑验收场景 1–6
  scripts/test.sh             起 Postgres+pgvector、Redis、Qdrant 容器后跑全部测试
  scripts/failover.sh         停 a → recover-worker → 以 WORKER=1 起 b
  scripts/live.sh             真实模型验收（场景 1–3），写 verification/*.json
  testdata/corpus/{goagent,goagent-contrib}/...   离线语料
  Dockerfile, docker-compose.yml, README.md, FINDINGS.md, verification/
```

---

### Task 1：contrib 修复版发 tag（document / retrieval / retrieval/qdrant）

规格写明"模块与 goagent v0.54.1 不兼容时，先修 contrib 并发版"。

- 已有 tag `document/v0.3.0` 和 `retrieval/v0.4.0` 引用了 goagent v0.53 已删除的 `utils/json`，无法编译。
- master `396d3eca4884` 已经修复。
- `retrieval/qdrant/v0.4.0` 依赖坏掉的 retrieval。

**Files：** 无代码改动。只在 contrib 仓库 origin/master `396d3eca4884` 上打附注 tag。

- [ ] **Step 1：在临时模块验证三者在 master 提交上与 goagent v0.54.1 同编译**

```bash
d=$(mktemp -d /tmp/ka-compat.XXXX) && cd $d && go mod init compat
cat > main.go <<'EOF'
package main
import (_ "github.com/kart-io/goagent-contrib/document"; _ "github.com/kart-io/goagent-contrib/retrieval"; _ "github.com/kart-io/goagent-contrib/retrieval/qdrant"; _ "github.com/kart-io/goagent-contrib/management/pgstore")
func main() {}
EOF
GOFLAGS=-mod=mod go get github.com/kart-io/goagent@v0.54.1 github.com/kart-io/goagent-contrib/document@396d3eca4884 github.com/kart-io/goagent-contrib/retrieval@396d3eca4884 github.com/kart-io/goagent-contrib/retrieval/qdrant@396d3eca4884 github.com/kart-io/goagent-contrib/management@396d3eca4884
go build ./... && go vet ./...
```

Expected：build 和 vet 无输出、退出码 0。这里的 `-mod=mod` 只用于临时目录里的 `go get`，不进仓库。

- [ ] **Step 2：征得用户同意后打 tag 并推送（contrib 主工作树不动，用 `git -C` 指定 origin/master 提交）**

```bash
cd /home/hellotalk/code/go/src/github.com/kart-io/goagent-contrib
for t in document/v0.3.1 retrieval/v0.4.1 retrieval/qdrant/v0.4.1; do git tag -a "$t" 396d3eca4884 -m "$t：修复与 goagent v0.53+ 的编译（utils/json 迁至 codec），要求 goagent v0.53.0"; done
git push origin document/v0.3.1 retrieval/v0.4.1 retrieval/qdrant/v0.4.1
```

Expected：`git ls-remote --tags origin | grep -E 'document/v0.3.1|retrieval/v0.4.1|qdrant/v0.4.1'` 输出 3 行。
同意前不推送；用户拒绝时改用 pseudo-version `@396d3eca4884`，并在 FINDINGS 记录。

---

### Task 2：模块骨架、配置与测试环境脚本

**Files：**
- Create：`products/knowledge-assistant/go.mod`
- Create：`internal/config/config.go`、`internal/config/config_test.go`
- Create：`scripts/test.sh`

**Interfaces：**
- Produces：
  - `config.Config`，字段如下：

    | 字段 | 类型 | 环境变量 | 规则 |
    | --- | --- | --- | --- |
    | `PGURL` | string | `KA_PG_URL` | 必填 |
    | `RedisAddr` | string | `KA_REDIS_ADDR` | 必填 |
    | `QdrantAddr` | string | `KA_QDRANT_ADDR` | gRPC `host:port`，必填 |
    | `DocsAlias` | string | `KA_DOCS_ALIAS` | 默认 `ka_docs` |
    | `Listen` | string | `KA_LISTEN` | 默认 `127.0.0.1:8081` |
    | `Worker` | bool | `WORKER` | 值为 `"1"` 时为真 |
    | `CacheTagKey` | []byte | `KA_CACHE_TAG_KEY` | hex 解码后 ≥32 字节，必填 |
    | `CachePrefix` | string | `KA_CACHE_PREFIX` | 默认 `ka:cache:` |
    | `Model` | string | `KA_MODEL` | `live`（默认）或 `script` |
    | `Embedder` | string | `KA_EMBEDDER` | `gemini`（默认）或 `hash` |
    | `CorpusDir` | string | `KA_CORPUS_DIR` | 默认 `/corpus`，其下每个子目录名即仓库名 |

  - `func FromEnv() (Config, error)`
  - `func FromLookup(get func(string) (string, bool)) (Config, error)`（供测试使用）。

- [ ] **Step 1：写失败测试** `TestFromLookup`，表驱动：
  - 缺 `KA_PG_URL` 时报错，信息含 `KA_PG_URL`。
  - `KA_CACHE_TAG_KEY` 是非法 hex，或解码后只有 31 字节时报错。
  - `KA_MODEL=foo` 报错。
  - 最小合法输入下，`DocsAlias=="ka_docs"`、`Listen=="127.0.0.1:8081"`、`Worker==false`、`Model=="live"`、`Embedder=="gemini"`。
  - `WORKER=1` 时 `Worker==true`。

- [ ] **Step 2：运行确认失败**

  Run：`cd products/knowledge-assistant && TMPDIR=/tmp taskset -c 2,3,6-27 go test ./internal/config/`

  Expected：FAIL（未定义 `FromLookup`）。

- [ ] **Step 3：实现。** `go.mod` 的 require 按 Global Constraints 写入版本，`go mod tidy` 在后续任务引入依赖时执行。

  `FromLookup` 的错误用 `fmt.Errorf("缺少环境变量 %s", name)` 这类中文信息。

- [ ] **Step 4：写 `scripts/test.sh`。** 模式照 contrib `scripts/test-pgstore.sh`：
  - 容器：`pgvector/pgvector:pg17`（`POSTGRES_PASSWORD=test`）、`redis:7-alpine`、`qdrant/qdrant:v1.20.2`。
  - 端口：Postgres `55440`、Redis `56390`、Qdrant gRPC `56334`，都绑 `127.0.0.1`。
  - 容器名带 `$$`，`trap` 中删除容器。
  - 等待 `pg_isready` 和 Qdrant `/readyz` 就绪；执行 `CREATE EXTENSION IF NOT EXISTS vector`。
  - 导出 `KA_TEST_PG_URL`、`KA_TEST_REDIS_ADDR`、`KA_TEST_QDRANT_ADDR`。
  - 执行 `go vet ./... && TMPDIR=/tmp taskset -c 2,3,6-27 go test -count=1 "$@" ./...`。

  需要外部服务的测试，在变量缺失时调用 `t.Skip("需经 scripts/test.sh 运行")`。

- [ ] **Step 5：运行**

  Run：`scripts/test.sh ./internal/config/`

  Expected：PASS。

- [ ] **Step 6：提交**

```bash
git add products/knowledge-assistant/go.mod products/knowledge-assistant/internal/config products/knowledge-assistant/scripts/test.sh
git commit -m "feat(knowledge-assistant): 模块骨架、环境变量配置与容器测试脚本"
```

---

### Task 3：嵌入器（Gemini 适配与确定性哈希）

**Files：**
- Create：`internal/embed/gemini.go`、`internal/embed/hash.go`、`internal/embed/embed_test.go`

**Interfaces：**
- Produces：
  - `func NewHash(dims int) *Hash`：实现 `retrieval.Embedder`，即 `Embed`、`EmbedBatch`、`Dimensions`。
  - `func NewGemini(ctx context.Context) (*Gemini, error)`：用 `providers.NewGeminiWithOptions()`，API key 与模型读 `GEMINI_API_KEY`、`GEMINI_EMBEDDING_MODEL`；构造时嵌入一次 `"维度探测"`，用返回长度作为 `Dimensions()`。
  - `func ModelID(e retrieval.Embedder) string`：Hash 返回 `"hash-v1@<dims>"`，Gemini 返回 `"<模型名>@<dims>"`。用作 `memory.Config.EmbedModel`。
- 哈希规则：
  - 分词：小写化后的 ASCII 词 `[a-z0-9_]+`，再加上每对相邻 CJK 字符组成的二元组。
  - 每个词经 FNV-1a 落到一个桶，按符号位 ±1 累加。
  - 没有词时第 0 维置 1，因为 `memory.NormalizeVector` 拒绝零向量。

- [ ] **Step 1：写失败测试**
  - `TestHashDeterministic`：同一文本两次嵌入结果相等，长度为 3072。
  - `TestHashSimilarity`：用余弦相似度比较。期望 `"pgstore 怎么建表"` 与 `"durable/pgstore 建表：执行 Schema"` 的相似度，大于它与 `"Redis 缓存前缀"` 的相似度。
  - `TestHashEmptyNotZero`：`Embed(ctx, "")` 至少有一维非 0。
  - `TestHashBatch`：`EmbedBatch` 与逐条 `Embed` 结果一致。
  - `TestGeminiLive`：未设置 `GEMINI_API_KEY` 时 `t.Skip`；设置时 `Dimensions()>0`，且 `ModelID` 以 `"gemini-embedding-001@"` 开头。

- [ ] **Step 2：运行确认失败**

  `go test ./internal/embed/` → FAIL。

- [ ] **Step 3：实现上述签名。** Gemini 的 `EmbedBatch` 逐条调用 `Embed`，因为 provider 没有批量接口，见 FINDINGS F6。

- [ ] **Step 4：运行确认通过**

  `TMPDIR=/tmp taskset -c 2,3,6-27 go test ./internal/embed/` → PASS（Gemini 用例在无 key 时 SKIP）。

- [ ] **Step 5：提交**

  `git commit -m "feat(knowledge-assistant): Gemini 嵌入适配与离线哈希嵌入器"`

---

### Task 4：文档采集与 Qdrant 索引（alias 蓝绿重建）

**Files：**
- Create：`internal/docs/collect.go`、`internal/docs/collect_test.go`
- Create：`internal/docs/index.go`、`internal/docs/index_test.go`
- Create：`testdata/corpus/goagent/memory/README.md`、`testdata/corpus/goagent/memory/doc.go`（一个带包注释和导出函数注释的小包）
- Create：`testdata/corpus/goagent-contrib/durable/pgstore/README.md`（含 `## 建表` 小节，写明执行 `Schema`）
- Create：`testdata/corpus/goagent-contrib/cache/redis/README.md`

**Interfaces：**
- Produces：
  - `type Chunk struct{ ID, Source, Text string }`
    - `Source` 形如 `goagent-contrib:durable/pgstore/README.md#建表`。
    - `ID` 是 UUIDv5，由 `Source` 加块序号派生，因为 Qdrant 的点 ID 只能是 UUID 或整数。
  - `func Collect(root string) ([]Chunk, error)`
    - `root` 下每个子目录是一个仓库，子目录名即仓库名。
    - Markdown 按 `#`–`####` 标题切成小节，再用 `document.NewRecursiveCharacterTextSplitter(document.RecursiveCharacterTextSplitterConfig{ChunkSize: 800, ChunkOverlap: 100})` 对每个小节调用 `SplitText`。
    - Go 包：对含非测试 `.go` 文件的目录用 `go/parser` 和 `go/doc` 解析，把包注释与导出符号注释拼成文本，`Source` 为 `<repo>:<包目录>#godoc`。
    - 跳过 `.git`、`vendor`、`testdata`、`node_modules`。
    - 每块 `Text` 第一行固定为 `来源：<Source>`，因为框架送给模型的证据不含元数据，见 FINDINGS F3。
  - `type Index struct`
  - `func NewIndex(ctx context.Context, addr, alias string, e retrieval.Embedder) (*Index, error)`：内部持有 `qdrant.NewClient`（go-client）。
  - `func (x *Index) Rebuild(ctx context.Context, key string, chunks []Chunk) (string, error)`
    - 新集合名为 `alias + "_" + hex(sha256(key))[:12]`。
    - 用 `cq.NewQdrantVectorStore(ctx, cq.QdrantConfig{URL: addr, CollectionName: 新名, VectorSize: e.Dimensions(), Embedder: e})` 加 `AddDocuments` 写入（`cq` 即 contrib `retrieval/qdrant`）。
    - 写入的元数据：`source`=Source，并原样带上 `Text`。
    - 用一次 `UpdateAliases` 原子地删旧 alias、建新 alias，再删除旧集合，返回新集合名。
    - 新集合已是 alias 目标时（重放），跳过写入，只清理残留的旧集合。
  - `func (x *Index) Current(ctx context.Context) (string, error)`：返回 alias 当前指向的集合；alias 不存在时报 `agentErrors.CodeNotFound`，信息含"先运行 ka ingest"。
  - `func (x *Index) Retriever(ctx context.Context, topK int) (*retrieval.RAGRetriever, error)`
    - 先调用 `Current` 校验 alias 存在。因为 `NewQdrantVectorStore` 会对不存在的名字隐式建集合，见 FINDINGS F5。
    - 再用 `CollectionName: alias` 构造 store。已实测 Qdrant 1.20 的 exists 和检索都会解析 alias。
    - 设置 `IncludeMetadata: true`、`MaxContentLength: 1200`。

- [ ] **Step 1：写失败测试（collect，无外部服务）**
  - `TestCollectMarkdownSections`：对 `testdata/corpus` 采集，存在 `Source=="goagent-contrib:durable/pgstore/README.md#建表"` 的块，其 `Text` 以 `"来源：goagent-contrib:durable/pgstore/README.md#建表\n"` 开头。
  - `TestCollectGodoc`：存在 `Source=="goagent:memory#godoc"` 的块，`Text` 含 doc.go 的包注释首句。
  - `TestCollectChunkSize`：任一块正文（去掉来源行）的 rune 数 ≤ 800。
  - `TestCollectStableIDs`：两次采集的 ID 序列相同。

- [ ] **Step 2：写失败测试（index，需 `KA_TEST_QDRANT_ADDR`）**

  alias 名用 `"t_" + 随机8位`，测试结束删除全部 `t_` 前缀的集合。
  - `TestRetrieverRequiresAlias`：alias 不存在时 `Retriever` 返回 `CodeNotFound`，并且 Qdrant 中没有生成与 alias 同名的集合。
  - `TestRebuildSwitchesAlias`：
    - 用 key `"k1"` 写入后，`Current` 等于返回名。
    - `Retriever(ctx,4).Retrieve(ctx,"pgstore 怎么建表")` 首条的 `Metadata["source"]` 前缀为 `goagent-contrib:durable/pgstore/`。
    - 再用 key `"k2"` 重建后，`Current` 变为新名，`k1` 对应的集合已不存在。
  - `TestRebuildIsIdempotent`：
    - 依次 `Rebuild("k1")`、`Rebuild("k2")`、`Rebuild("k2")`。
    - 第三次返回值与第二次相同。
    - 前缀为 alias 的集合只剩 1 个。
    - 第三次没有重写点：先记录点数，删除一个点后再重放，点数不恢复。

- [ ] **Step 3：运行确认失败**

  `scripts/test.sh ./internal/docs/` → FAIL。

- [ ] **Step 4：实现上述签名**

- [ ] **Step 5：运行确认通过**

  `scripts/test.sh ./internal/docs/` → PASS。

- [ ] **Step 6：提交**

  `git commit -m "feat(knowledge-assistant): 文档按小节采集与 Qdrant alias 蓝绿重建"`

---

### Task 5：对话模型（拷贝 livemodel + 脚本模型）

**Files：**
- Create：`internal/chat/livemodel.go`、`internal/chat/minimax.go`

  用 `git show HEAD:internal/livemodel/livemodel.go` 和 `git show HEAD:internal/livemodel/minimax.go` 取内容，包名改为 `chat`，文件头注释说明来源。不能 import 的原因见 FINDINGS F8。
- Create：`internal/chat/script.go`、`internal/chat/script_test.go`

**Interfaces：**
- Produces：
  - `func FromEnv() (*Client, error)`：拷贝而来。
  - `func NewScript() *Script`：实现 `llm.ToolStreamClient`，写法参考 contrib `management/examples/host/main.go:35-70`。

- 脚本模型规则。先看最后一条消息：
  - 是 `tool` 消息时，回答 `"工具结果：" + 内容`。内容含"拒绝"时，回答 `"用户拒绝，未清空"`。
  - 否则看最后一条 user 消息：
    - 以 `记住：` 开头：调用 `memory_save`，参数为 `{"content":<冒号后文本>,"kind":"fact"}`。
    - 含 `清空我的记忆`：调用 `purge_my_memory`，参数为 `{}`。
    - 含 `重建文档索引`：调用 `rebuild_docs_index`，参数为 `{}`。
    - 其他：回答 `"回显：\n" + 全部非 user、非 tool 消息正文按 "\n" 连接`。
  - 每次返回 `Usage{PromptTokens:3, CompletionTokens:2, TotalTokens:5}`。

- [ ] **Step 1：写失败测试** `TestScriptRules`，表驱动 5 种输入，断言 `FinishReason`、工具名、参数 JSON 或回答文本：
  - 回显用例：system 消息含 `"来源：goagent:memory#godoc"`，回答需包含该字符串。
  - 拒绝用例：tool 消息为 `"审批人已拒绝此操作"`，回答等于 `"用户拒绝，未清空"`。

- [ ] **Step 2：运行确认失败。**

  Run：`TMPDIR=/tmp taskset -c 2,3,6-27 go test ./internal/chat/`

  Expected：FAIL。

- [ ] **Step 3：实现。**

- [ ] **Step 4：运行确认通过。**

  Run：同 Step 2。

  Expected：PASS。拷贝来的 livemodel 测试不搬，它依赖仓库内 fixture。

- [ ] **Step 5：提交。**

  `git commit -m "feat(knowledge-assistant): 拷贝 livemodel 并加入离线脚本模型"`

---

### Task 6：存储装配、Factory 与按用户的 Agent

**Files：**
- Create：`internal/assistant/stores.go`、`internal/assistant/factory.go`、`internal/assistant/agent.go`
- Create：`internal/assistant/assistant_test.go`

**Interfaces：**
- Consumes：
  - `config.Config`
  - `embed.ModelID`、`retrieval.Embedder`
  - `docs.Index`（`Retriever`、`Rebuild`）、`docs.Collect`
  - `chat.FromEnv`、`chat.NewScript`
- Produces：
  - `func Migrate(ctx context.Context, pool *pgxpool.Pool) error`，在一个事务内按顺序：
    1. 取 `pg_advisory_xact_lock(80261116)`；
    2. 若 `to_regclass('goagent_durable_schema')` 为空，执行 durable/pgstore 的 `Schema+SessionSchema`；
    3. 若 `goagent_memory_vectors_schema` 为空，执行 pgvector 的 `Schema`；
    4. 建表 `ka_tool_runs(execution_key text NOT NULL, tool text NOT NULL, tenant text NOT NULL, at timestamptz NOT NULL DEFAULT now())`，只追加，用于断言工具执行次数。

    事务外再调用 `mgpg.Migrate(ctx, pool)`。整个过程可重复执行。
  - `type Stores struct{ Pool *pgxpool.Pool; Journal *pgstore.Store; Sessions *pgstore.SessionStore; Manage *mgpg.Store; Memory *memory.Memory; Cache builder.ModelCache; Redis goredis.UniversalClient }`
  - `func Open(ctx context.Context, c config.Config, model llm.Client, e retrieval.Embedder) (*Stores, error)` 和 `func (s *Stores) Close(ctx context.Context) error`。装配要点：
    - `Cache = builder.ModelCache{Cache: redis.New(...Prefix: c.CachePrefix), TTL: 4*time.Minute, Erase: builder.EraseRequired, TagKey: c.CacheTagKey}`。
    - `Memory = memory.New(memory.Config{Durable: Journal, Index: pgvector.New(...), Embedder: e, EmbedModel: embed.ModelID(e), OnErase: Cache.Eraser()})`。
    - `c.Model=="live"` 时再设置 `Model: model, Sessions: Sessions` 启用抽取；`script` 模式不抽取。
  - `const RunnerKind = "knowledge"`、`ImplementationVersion = "ka-1"`、`AgentName = "knowledge-assistant"`、`AgentVersion = "v1"`
  - `type Deps struct{ Stores *Stores; Model llm.ToolStreamClient; Index *docs.Index; CorpusDir string; Embedder retrieval.Embedder }`
  - `func Factory(d Deps) mg.Factory`
    - 返回包装类型，它的 `Validate` 委托给内部的 `adapters.NativeFactory`。
    - `Build` 先委托，再把 `Binding.Runtime` 包装成 `nsRuntime`。`nsRuntime` 的 `Run` 和 `Inspect` 都先执行 `ctx = memory.WithNamespace(ctx, spec.Task.TenantID)`，原因见 FINDINGS F1。
    - `NativeFactory.Check` 只接受 `ImplementationVersion == "ka-1"`，否则返回 `agentErrors.ErrManagementUnsupported`。
    - `BuildAgent` 返回：

      ```
      builder.NewSimpleBuilder(d.Model)
        .WithSystemPrompt(系统提示词)
        .WithRetriever(retriever, builder.RetrievalConfig{MaxDocuments: 4})
        .WithLongTermMemory(d.Stores.Memory)
        .WithModelCache(d.Stores.Cache)
        .WithTools(dispatcher.Tools()...)
        .WithMaxIterations(6)
      ```

      系统提示词要求：用中文回答；逐条写出所依据资料第一行的"来源："路径；没有证据时直说。
  - 工具。`dispatcher` 用 `tools.NewDispatcher(registry, tools.DispatchConfig{Pipeline: &tools.Pipeline{Pre: []tools.PreHook{approvalHook}}, Approver: ap})` 构造。registry 的内容如下：

    | 工具 | 行为 | 审批 |
    | --- | --- | --- |
    | `d.Stores.Memory.Tools()` 全部 | memory 自带的检索、保存、遗忘 | 不需要 |
    | `purge_my_memory` | 先插入 `ka_tool_runs(in.ExecutionKey,'purge',tenant)`，再 `Memory.Purge(ctx)`；结果为 `"已清空当前用户的全部记忆"` | 需要 |
    | `rebuild_docs_index` | 先插入 `ka_tool_runs`，再 `d.Index.Rebuild(ctx, in.ExecutionKey, docs.Collect(d.CorpusDir))`；结果为新集合名 | 需要 |

    `approvalHook` 只对后两个工具名返回 `tools.PreDecision{Kind: tools.PreAsk, Feedback: "<操作说明>需要人工审批"}`，其余返回 `PreAllow`。
  - `func EnsureAgent(ctx context.Context, s *mg.Service, a mg.Actor) (mg.Agent, error)`
    - 依次 `RegisterAgent`、`SaveVersion{Version: "v1", RunnerKind: "knowledge", ImplementationVersion: "ka-1", Config: {}}`、`PublishVersion`。
    - 幂等键固定为 `ka-register`、`ka-save-v1`、`ka-publish-v1`。幂等记录按租户隔离，重复调用返回同一 Agent。

- [ ] **Step 1：写失败测试**（需要 `KA_TEST_PG_URL` 和 `KA_TEST_REDIS_ADDR`；模型用 `chat.NewScript()`，嵌入用 `embed.NewHash(3072)`；docs.Index 用 Task 4 的测试 alias，并在测试内先 `Rebuild` 一次）
  - `TestMigrateTwice`：连续两次 `Migrate` 都返回 nil。
  - `TestEnsureAgentIdempotent`：同一 actor 调用两次，得到相同的 `Agent.ID`；两个不同 actor 得到不同的 `Agent.ID`。
  - `TestNamespaceBoundInRun`：
    - 构造 `mg.New(...)`，Factories 为 `{"knowledge": Factory(deps)}`，Authorizer 为 `mg.TenantAuthorizer{}`；起 Worker。
    - u1 提交 `"记住：我们用 pgstore"`，等待任务 `succeeded`。
    - 用 `memory.WithNamespace(ctx,"u1")` 调用 `Recall`，能命中这条记录。
    - 用 `"u2"` 命名空间调用 `Recall`，结果为空。
  - `TestPurgeNeedsApproval`：
    - u1 提交 `"清空我的记忆"`，任务进入 `waiting_approval`。
    - `ListApprovals` 得到 1 条 pending 记录。
    - 批准后任务 `succeeded`，`ka_tool_runs` 中该任务的 purge 行数为 1，`Pinned` 和 `Recall` 均为空。

- [ ] **Step 2：运行确认失败。**

  Run：`scripts/test.sh ./internal/assistant/`

  Expected：FAIL。

- [ ] **Step 3：实现。**

- [ ] **Step 4：运行确认通过。**

  Run：`scripts/test.sh ./internal/assistant/`

  Expected：PASS。

- [ ] **Step 5：提交。**

  `git commit -m "feat(knowledge-assistant): 存储装配、命名空间绑定的 Factory 与审批工具"`

---

### Task 7：HTTP 入口、产品接口、静态页面与 `ka` 命令

**Files：**
- Create：`internal/web/server.go`、`internal/web/server_test.go`、`internal/web/static/index.html`、`app.js`、`style.css`
- Create：`cmd/ka/main.go`

**Interfaces：**
- Produces：
  - `func ActorFromRequest(r *http.Request) (mg.Actor, error)`
    - 解析 `Bearer <name>`，`name` 须匹配 `^[a-z0-9][a-z0-9_-]{0,63}$`，否则返回 `agentErrors.ErrManagementUnauthenticated`。
    - 返回 `mg.Actor{TenantID: name, Subject: name}`。
  - `func NewHandler(s *mg.Service, st *assistant.Stores, idx *docs.Index) (http.Handler, error)`，路由：
    - `/v1/` → `httpapi.New(s, httpapi.Config{ActorFromRequest: ActorFromRequest})`
    - `POST /api/agent` → `assistant.EnsureAgent`，返回 `{"agent_id","version"}`
    - `GET /api/memories?q=` → 在用户命名空间下返回 `{"pinned":Memory.Pinned,"matches":Memory.Recall(Query{Text:q,K:20})}`；`q` 为空时只返回 pinned。memory 没有 List，见 FINDINGS F4。
    - `DELETE /api/memories/{id}` → `Memory.Forget`，返回 204；找不到时返回 404。
    - `GET /api/sources?q=` → `idx.Retriever(ctx,4).Retrieve`，返回 `[{"source","score","excerpt"}]`
    - `/` → 静态页面（`//go:embed static`）
  - 页面有三个区域：
    - 对话：建会话，提交任务，每秒轮询一次任务状态，显示结果。
    - 待审批：`GET /v1/approvals?status=pending`，提供批准和拒绝。
    - 我的记忆：搜索框、置顶列表、删除按钮。

    只调用上面这些接口，写法参考 `orchestration/httpagent/web`。
  - `cmd/ka`：
    - `ka migrate`
    - `ka ingest`：`Collect(CorpusDir)` 后执行 `Rebuild(key="ingest-"+语料哈希)`。
    - `ka serve`：`WORKER=1` 时运行 `mg.NewWorker(svc).Run`；`KA_MODEL=live` 时额外运行 `Memory.Run`。
    - `ka recover-worker -reason <文本> -confirm-stopped`：
      - 从 `Manage.Get(ctx,"workers","__system","default")` 解码出 `mg.WorkerOwner`，因为框架没有读取接口，见 FINDINGS F2。
      - 未给 `-confirm-stopped` 时，`ConfirmWorkerStopped` 返回错误"须由运维确认旧实例已停止"。
      - 以 `mg.Actor{TenantID:"ops",Subject:"ops"}` 调用 `RecoverWorkerSlot`。
    - 关闭顺序照 host 示例：`BeginShutdown`、取消 worker ctx、`server.Shutdown`、`worker.Shutdown`、`Stores.Close`。

- [ ] **Step 1：写失败测试**
  - `TestActorFromRequest`：表驱动以下输入：
    - 无头、`Bearer `、`Bearer a b`、`Bearer ../x`、65 字节名、`Basic u1` → 都返回 `ErrManagementUnauthenticated`。
    - `Bearer u1` → `Actor{u1,u1}`。
  - `TestMemoryRoutes`（需服务）：
    - u1 经 `/v1` 提交 `"记住：我偏好中文示例"` 并完成。
    - `GET /api/memories?q=中文示例` 的 matches 含该条记录。
    - `DELETE` 该 id 返回 204。
    - 再 GET，matches 不含该记录。
    - 用 u2 的 token 删除同一个 id 返回 404。
  - `TestStaticIndex`：`GET /` 返回 200，正文含 `<title>团队知识助手</title>`。

- [ ] **Step 2：运行确认失败。**

  Run：`scripts/test.sh ./internal/web/`

  Expected：FAIL。

- [ ] **Step 3：实现。**

- [ ] **Step 4：运行确认通过，并编译二进制。**

  Run：`scripts/test.sh ./internal/web/ && go build ./cmd/ka`

  Expected：PASS。

- [ ] **Step 5：提交。**

  `git commit -m "feat(knowledge-assistant): HTTP 入口、记忆与来源接口、静态页面与 ka 命令"`

---

### Task 8：离线验收场景（双实例子进程）

**Files：**
- Create：`e2e/e2e_test.go`

**Interfaces：**
- Consumes：`ka` 二进制（`TestMain` 中 `go build -o $TMP/ka ./cmd/ka`）、Task 2 的测试环境变量。
- 测试夹具：
  - `start(t, name string, worker bool) *proc`：
    - 以子进程运行 `ka serve`。
    - 环境变量：`KA_MODEL=script`、`KA_EMBEDDER=hash`、`KA_CORPUS_DIR=testdata/corpus`、`KA_DOCS_ALIAS`=测试随机 alias、`KA_CACHE_PREFIX`=测试随机前缀、`KA_CACHE_TAG_KEY`=固定 32 字节 hex、`WORKER`。
    - 监听随机端口，等待 `GET /` 返回 200 才返回。
  - `(*proc).Kill()`：SIGKILL，不走优雅退出。
  - `ask(t, base, user, text) Task`：依次 `POST /api/agent`、建会话（轮询到 ready）、提交任务、轮询到终态或 `waiting_approval`。
  - 场景开始前先跑一次 `ka migrate` 和 `ka ingest`。

- [ ] **Step 1：写场景测试**（规格第 6 节 1–6，每个一个 `t.Run`，共用 a(WORKER=1)、b(WORKER=0)）
  1. `TestRAGCitesSources`：经 a 让 u1 问"pgstore 怎么建表"，任务结果含 `goagent-contrib:durable/pgstore/`。
  2. `TestMemoryIsolatedPerUser`：
     - u1 在 a 上说"记住：我们用 pgstore"。
     - u1 新会话在 b 上问"存储选型用什么"，回显含"我们用 pgstore"。
     - u2 在 b 上问同一问题，回显不含"我们用 pgstore"。
  3. `TestForgetAcrossInstancesErasesCache`：
     - u3 在 a 上写入记忆，然后在 a 上问一次问题，使 Redis 中产生缓存条目。
     - 记录 SCAN `<前缀>*` 新增的键集合 K，要求 K 非空。
     - 经 b 的 `GET /api/memories` 能查到这条记忆。
     - 经 b 的 `DELETE` 删除后：a 的 `GET /api/memories?q=` 不含它；K 中所有键在 Redis 中都已不存在（EXISTS=0）。
  4. `TestApprovalSurvivesKillAndFailover`：
     - u1 在 a 上提交"清空我的记忆"，状态进入 `waiting_approval`。
     - `a.Kill()`。
     - 执行 `ka recover-worker -reason e2e -confirm-stopped`，退出码 0。
     - 以 WORKER=1 重启 b。
     - 在 b 上批准该审批，任务 `succeeded`。
     - `ka_tool_runs` 中该任务 ExecutionKey 的 purge 行数为 1。
     - 再在 b 上召回 u1 的记忆，结果为空。
  5. `TestRejectKeepsMemory`：
     - u4 写入记忆，然后提交清空请求。
     - 拒绝该审批，任务 `succeeded`，结果含"用户拒绝，未清空"。
     - 记忆仍可召回；`ka_tool_runs` 无该任务的行。
  6. `TestRebuildIndexViaApproval`：
     - 记录 `Current()` 为 old。
     - u1 提交"重建文档索引"，批准后任务 `succeeded`。
     - `Current()` 不等于 old，且 old 集合已不存在。
     - 切换后再问场景 1 的问题，结果仍含来源前缀。

- [ ] **Step 2：运行，并对每个失败场景定位根因。**
  - 根因在产品代码：修产品代码。
  - 根因在框架：写入 FINDINGS，并按规格决定修框架还是标为已知限制，不在产品里绕过。

  Run：`scripts/test.sh ./e2e/ -v`

  Expected：6 个子测试全部 PASS。

- [ ] **Step 3：提交。**

  `git commit -m "test(knowledge-assistant): 双实例离线验收场景 1–6"`

---

### Task 9：部署、真实模型验收与文档

**Files：**
- Create：`Dockerfile`、`docker-compose.yml`、`scripts/failover.sh`、`scripts/live.sh`、`README.md`、`FINDINGS.md`、`verification/.gitkeep`
- Modify：仓库根 `README.md` 目录表，加一行 `products/knowledge-assistant`。

**内容约束：**

- **Dockerfile**：多阶段构建。
  - 构建阶段编译 `ka`。
  - 语料阶段按固定版本克隆两个仓库，放到 `/corpus/goagent` 和 `/corpus/goagent-contrib`：
    - goagent：`git clone --depth 1 -b v0.54.1 https://github.com/kart-io/goagent`
    - contrib：先 `git clone`，再 `git checkout 396d3eca4884`
  - 运行镜像包含 `ka` 和 `/corpus`。
- **docker-compose.yml**：
  - 服务：
    - `postgres`：`pgvector/pgvector:pg17`，用 init SQL 执行 `CREATE EXTENSION vector`。
    - `redis`：`redis:7-alpine`。
    - `qdrant`：`qdrant/qdrant:v1.20.2`。
    - `migrate`：执行 `ka migrate`。
    - `ingest`：执行 `ka ingest`，依赖 migrate 成功完成。
    - `a`：执行 `ka serve`，`WORKER=1`，端口 `8081`。
    - `b`：执行 `ka serve`，`WORKER=0`，端口 `8082`。
  - `a` 和 `b` 依赖 `ingest` 成功完成。
  - 通过 `.env` 传入 `GEMINI_API_KEY`、`GOAGENT_LIVE_PROVIDER`、对应模型 key、`KA_CACHE_TAG_KEY`。
- **scripts/failover.sh**：
  1. `docker compose kill a`；
  2. 确认 `docker compose ps -q --status running a` 为空，非空时退出 1；
  3. `docker compose run --rm b ka recover-worker -reason "$1" -confirm-stopped`；
  4. `WORKER=1 docker compose up -d --no-deps b`。
- **scripts/live.sh**：
  - 未设置 `KA_LIVE=1` 时提示后退出 0。
  - 否则 `docker compose up -d` 并等待 a、b 就绪，然后跑固定问答：
    - 3 个文档问题，各带期望路径前缀：
      - `pgstore 怎么建表` → `goagent-contrib:durable/pgstore/`
      - `模型缓存怎么在删除记忆后擦除` → `goagent:`
      - `Redis 缓存前缀默认是什么` → `goagent-contrib:cache/redis/`
    - 记忆的写入、跨实例召回和删除，对应场景 2–3。
  - 任一失败条件成立，就以非零退出：
    - 有问题的回答中缺少期望的路径前缀；
    - 记忆没有召回；
    - 删除后仍能召回。
  - 报告写入 `verification/live-<UTC时间>.json`，每题记录：`question`、`expected_prefix`、`sources_in_answer`、`recalled`、`tokens`（从任务 Run 统计读取）、`elapsed_ms`。
- **README.md**：
  - 按"怎么搭一个这样的产品"组织：架构图（文字）、每个组件为什么这样选、启动步骤、多实例语义与故障转移、生产须替换的部分（认证）。
  - 写明取值理由：切分 800/100、召回 4 条、缓存 TTL 4 分钟（须 ≤ `EraseWithin` 5 分钟）。
- **FINDINGS.md**：
  - 每条写现象、证据（文件:行或测试名）、影响、产品中的处理方式、建议（修框架或已知限制）。
  - 至少包含 F1–F10，执行中新发现的问题顺延编号：
    - F1：`management` 不绑定记忆命名空间，宿主须包装 Factory（`management/adapters/native.go` 的 Run/Inspect 直接透传 ctx）。
    - F2：没有读取当前 Worker owner/generation 的接口，故障转移须直接读 `workers` 文档；owner 是 uuid，无法对应到实例。
    - F3：原生适配器的 Run 丢弃 `sources` 事件，并且证据块不含 `source` 元数据（`builder/retrieval.go:90-94`），产品只能把来源写进正文。
    - F4：memory 没有 List，"查看我的记忆"只能用搜索加置顶。
    - F5：Qdrant store 对不存在的名字隐式建集合，不校验已有集合的维度，没有 alias 支持；Embedder 为 nil 时退回假向量。
    - F6：Gemini provider 不满足 `retrieval.Embedder`（缺 EmbedBatch/Dimensions），维度须探测。
    - F7：`MarkdownTextSplitter`、`TokenTextSplitter`、`CodeTextSplitter` 经 `SplitDocuments` 不切分（嵌入无虚派发，`document/splitter.go:83`）；Markdown 切分不保留标题。
    - F8：`internal/livemodel` 无法被嵌套模块复用。
    - F9：tag `document/v0.3.0`、`retrieval/v0.4.0` 与 goagent ≥v0.53 不兼容（已在 Task 1 发补丁版）；`management` 模块无 tag。
    - F10：会话不记录创建者，按用户隔离只能每用户一个租户，Agent 也须按用户注册。
  - 后续工作：文档向量改存 Postgres（规格方案 B）。

- [ ] **Step 1：写以上文件。**

- [ ] **Step 2：验证 compose 能起来，并演练故障转移。**

  Run：
  ```bash
  cd products/knowledge-assistant
  KA_MODEL=script KA_EMBEDDER=hash docker compose up -d --build
  curl -sf localhost:8081/ >/dev/null
  curl -sf localhost:8082/ >/dev/null
  scripts/failover.sh 演练
  ```

  Expected：两个 curl 都返回 0；failover 脚本退出 0；之后 b 的日志中出现 Worker 启动信息。

- [ ] **Step 3：跑真实模型验收。**

  条件：本机有 `GEMINI_API_KEY` 和对话模型 key 时执行。

  Run：`KA_LIVE=1 scripts/live.sh`

  Expected：退出码 0，`verification/` 下生成报告。没有 key 时，在 FINDINGS 的"验证状态"段记录"真实模型路径未跑"，并且不宣称完成。规格把"只在模拟模型下验证"列为反目标，所以缺 key 时要向用户索取后补跑。

- [ ] **Step 4：运行全部离线测试。**

  Run：`scripts/test.sh ./...`

  Expected：全部 PASS。

- [ ] **Step 5：提交。**

  只 add `products/knowledge-assistant/` 下的新文件，以及根 `README.md`：

  ```bash
  git commit -m "docs(knowledge-assistant): 部署编排、故障转移与真实模型验收脚本、搭建指南与框架发现"
  ```
