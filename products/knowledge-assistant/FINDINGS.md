# 框架发现与已知限制

组装这个产品时暴露的 goagent / goagent-contrib 问题。按规格的反目标，产品不复制、不绕过框架逻辑。能在产品侧正当解决的（例如补上缺失的接口声明）就解决并写明；属于框架的，写清建议是修框架还是作为已知限制保留。

版本：goagent `v0.54.1`；contrib `durable/pgstore v0.5.1-0.20261116130024-396d3eca4884`、`management v0.0.0-20261116130024-396d3eca4884`、`cache/redis v0.1.0`、`memory/pgvector v0.1.1`、`document v0.3.1`、`retrieval v0.4.1`、`retrieval/qdrant v0.4.1`。

行号对应上述版本。`goagent:` 指 goagent 模块，`contrib:` 指 goagent-contrib 对应子模块；没有前缀的路径相对本目录。

## 总览

| 编号 | 问题 | 严重度 | 建议 |
| --- | --- | --- | --- |
| F1 | management 不绑定记忆命名空间 | 中 | 修框架 |
| F2 | 无法读取 Worker 槽位持有者 | 中 | 修框架 |
| F3 | 原生适配器丢弃来源，证据块不含来源 | 中 | 修框架 |
| F4 | memory 没有列举接口 | 低 | 修框架 |
| F5 | Qdrant store 隐式建集合、不校验维度、无 alias、nil 嵌入器退回假向量 | 中 | 修框架 |
| F6 | Gemini provider 不满足 `retrieval.Embedder` | 低 | 修框架 |
| F7 | 多个切分器经 `SplitDocuments` 不切分；Markdown 切分丢标题 | 中 | 修框架 |
| F8 | `internal/livemodel` 不能被嵌套模块复用 | 低 | 已知限制 |
| F9 | contrib 旧 tag 与 goagent ≥v0.53 不兼容；management 无 tag | 中 | 已修一半 |
| F10 | 会话不记录创建者 | 中 | 修框架 |
| F11 | RAGRetriever 不实现 `DocumentRetriever` | 低 | 修框架 |
| F12 | Gemini Embed 拒绝空文本 | 低 | 已知限制 |
| F13 | 每个 Qdrant store 自建 gRPC 客户端，检索器无法关闭 | 低 | 修框架 |
| F14 | Qdrant go-client 与服务端版本不匹配，每次连接打 WARN | 低 | 修框架 |
| F15 | pgstore tag 满足不了 management 的依赖 | 中 | 修框架（发版） |
| F16 | 请求分隔符未导出，召回块排在请求之后 | 低 | 修框架 |
| F17 | 工具的 `ExecutionKey` 无法映射回 Task | 低 | 修框架 |
| F18 | management 不保留执行错误原文 | 中 | 修框架 |
| F19 | Worker 认领槽位途中被取消会泄漏槽位 | **高** | 修框架 |
| F20 | 任意租户的 Actor 都能回收 Worker 槽位 | 中 | 修框架或写明 |
| F21 | `memory.Scored` 的分数字段没有 json tag | 低 | 修框架 |
| F22 | 模型客户端未声明缓存身份时模型缓存静默失效 | **高** | 修框架 |
| F23 | cache/redis 的会话标签键在遗忘后保留到过期 | 低 | 写明语义 |
| F24 | 运维命令也须装配完整 Factory | 低 | 修框架 |

---

## F1 management 不把任务租户绑定为记忆命名空间

- **现象**：长期记忆按 `memory.WithNamespace(ctx, …)` 隔离，但 management 执行任务时不设置命名空间。不处理的话，所有用户的记忆写进同一个命名空间。
- **证据**：`goagent:management/adapters/native.go:63`（Run）、`:96`（Inspect）把 ctx 原样传给 Agent。
- **影响**：凡是"management + 长期记忆"的宿主都要自己包一层，漏掉就会让用户之间的记忆串在一起。
- **产品处理**：`internal/assistant/factory.go:53-63` 包装 Factory，Run 与 Inspect 都以 `spec.Task.TenantID` 绑定命名空间。Inspect 回放也必须绑定，否则长期记忆会检测到命名空间变化而失败。
- **建议**：修框架。`NativeFactory` 增加 `Namespace func(mg.RunSpec) []string`，默认取租户。

## F2 没有读取当前 Worker 槽位持有者的接口

- **现象**：`RecoverWorkerSlot` 要求传入 owner 与 generation，management 却没有公开读取它们的方法。
- **证据**：`cmd/ka/main.go:284` 只能直接 `Store.Get("workers", "__system", "default")` 并解码内部类型 `WorkerOwner`。
- **影响**：故障转移工具依赖存储里的文档布局。另外 owner 是进程随机 uuid，运维无法从它判断属于哪个实例，"确认旧实例已停止"只能凭部署拓扑推断。
- **产品处理**：如上直接读取；`scripts/failover.sh` 先确认容器 a 不在运行，再回收。
- **建议**：修框架。提供 `Service.WorkerSlot(ctx, actor)`，并允许 Worker 声明可读的实例标识（主机名、Pod 名）写入槽位。

## F3 原生适配器丢弃 sources 事件，证据块不含来源

- **现象**：Builder 在检索时生成了带 `Source` 的 `RetrievalSource`，但送给模型的证据只有编号和正文；management 原生适配器只收集 Error 和 Output 事件，sources 事件被丢弃，任务结果里也没有来源。
- **证据**：`goagent:builder/retrieval.go:90-94`（`fmt.Fprintf(&evidence, "[%d]\n%s\n\n", …)` 不写 source）；`goagent:management/adapters/native.go:76-83`。
- **影响**：要求"回答附来源"的产品拿不到结构化来源，只能依赖模型照抄正文。
- **产品处理**：ingest 在每块正文首行写 `来源：repo:path#小节`（`internal/docs/collect.go`），系统提示词要求模型逐条写出。另外提供 `/api/sources` 直接查检索结果。
- **建议**：修框架。证据块带上 source 元数据；RunResult 增加 sources 字段并经 httpapi 返回。

## F4 memory 没有列举接口

- **现象**：只有 `Recall`（按查询）和 `Pinned`，没有按命名空间分页列出全部记忆。
- **证据**：`goagent:memory/memory.go` 的导出方法（`Recall` :469、`Pinned` :541，没有 List）。
- **影响**："查看我的全部记忆"做不出来；用户无法确认某条记忆是否还在，只能换关键词搜索。
- **产品处理**：`internal/web/server.go:103` 用"置顶 + 按 q 召回 20 条"拼出"我的记忆"。
- **建议**：修框架。增加 `List(ctx, cursor, limit)`。

## F5 Qdrant store：隐式建集合、不校验维度、不支持 alias、nil 嵌入器退回假向量

- **现象**：
  - 名字不存在时自动建集合。以 alias 名打开 store 时，若 alias 尚未创建，就会建出一个与 alias 同名的真实集合，之后再创建这个 alias 会失败。
  - 集合已存在时不校验维度。
  - 没有 alias 操作。
  - `Embedder` 为 nil 时静默改用 `SimpleEmbedder` 假向量。
- **证据**：`contrib:retrieval/qdrant/vector_store_qdrant.go:74-76`（nil 嵌入器）、`:124-149`（`ensureCollection`）。
- **影响**：
  - 嵌入模型改了维度时写入或检索才报错，或者结果直接错乱。
  - 漏传嵌入器的检索结果毫无意义，却不报错。
  - 蓝绿重建只能绕开 store 直接用 go-client。
- **产品处理**：`internal/docs/index.go` 用 go-client 自管集合与 alias（`:79-114`）。`Retriever` 先经 `Current` 确认 alias 存在，再以 alias 名构造 store，不给它隐式建集合的机会。
- **建议**：修框架。增加 `CreateIfMissing` 开关与维度校验；nil 嵌入器直接报错；提供 alias 读写或至少支持以 alias 打开。

## F6 Gemini provider 不满足 `retrieval.Embedder`

- **现象**：`GeminiProvider` 只有 `Embed`，缺 `EmbedBatch` 和 `Dimensions`，也没有用 Gemini 的批量嵌入 API。
- **证据**：`internal/embed/gemini.go` 适配层；`NewGemini` 启动时嵌入一次探测文本来取得维度。
- **影响**：
  - 每个宿主都要写适配器，启动多一次网络调用。
  - 逐条嵌入很慢：完整语料约 5500 块，串行每条 0.5–0.9 秒，一次 ingest 要一小时以上。
- **产品处理**：适配器在产品内实现，`EmbedBatch` 8 并发（提交 9927f15）。
- **建议**：修框架。provider 实现 `retrieval.Embedder`，用 `batchEmbedContents`，维度按模型声明或首次调用时缓存。

## F7 多个切分器经 `SplitDocuments` 不切分；Markdown 切分不保留标题

- **现象**：`MarkdownTextSplitter`、`TokenTextSplitter`、`CodeTextSplitter` 内嵌 `BaseTextSplitter`。`SplitDocuments` 定义在基类上，调用的是基类的 `SplitText`，而 Go 的嵌入没有虚派发，子类的切分逻辑因此被跳过。另外，Markdown 切分不在块里保留所属标题。
- **证据**：`contrib:document/splitter.go:83-87`。
- **影响**：按文档切分时整篇只得到一块，检索质量和来源精度都会出问题，而且不报错。
- **产品处理**：`internal/docs/collect.go:105` 起自行按标题切出小节，再用 `RecursiveCharacterTextSplitter.SplitText` 切块，来源精确到小节。
- **建议**：修框架。各切分器自己实现 `SplitDocuments`，或基类持有切分函数；Markdown 切分在元数据里写入标题路径。

## F8 `internal/livemodel` 不能被嵌套模块复用

- **现象**：仓库根模块的 `internal/livemodel` 不能被 `products/knowledge-assistant` 这个独立 module 导入。
- **证据**：`internal/chat/livemodel.go:1-2`、`internal/chat/minimax.go:1`。
- **影响**：两份代码会漂移。本产品已经比原文件多了 `ModelName`（见 F22）。
- **产品处理**：拷贝，并在文件头写明来源和偏离之处。
- **建议**：已知限制。若真实模型客户端要成为公共能力，应移到 goagent 或独立 module。

## F9 contrib 旧 tag 与 goagent ≥v0.53 不兼容；management 无 tag

- **现象**：`document/v0.3.0`、`retrieval/v0.4.0` 引用了 goagent v0.53 删除的 `utils/json`，编译失败。`management` 模块没有任何 tag。
- **证据**：`go.mod`。为此补发了 `document/v0.3.1`、`retrieval/v0.4.1`、`retrieval/qdrant/v0.4.1`（指向 contrib 396d3ec）；management 只能用伪版本 `v0.0.0-20261116130024-396d3eca4884`。
- **影响**：按文档用最新 tag 的用户无法编译；用伪版本的依赖无法表达兼容性承诺。
- **产品处理**：用补丁版和伪版本。
- **建议**：前一半已修。management 需要打 tag；CI 应校验各子模块的最新 tag 能与当前 goagent 一起编译。

## F10 会话不记录创建者

- **现象**：`management.Session` 只有租户，没有创建者主体。
- **证据**：`goagent:management/types.go:89-100`（Session 字段），`:65-70`（Meta 只有 TenantID）。
- **影响**：同一租户内无法按用户隔离会话；要做到按用户隔离，只能每用户一个租户，Agent 也要按用户注册。
- **产品处理**：`Actor.TenantID = Actor.Subject = 用户名`，`assistant.EnsureAgent` 按租户幂等注册。
- **建议**：修框架。Session 记录创建者 Subject，Authorizer 的 Scope 可以按它过滤。

## F11 RAGRetriever 不实现 `interfaces.DocumentRetriever`

- **现象**：`retrieval.RAGRetriever` 只有 `Retrieve(ctx, string)`，没有 `GetRelevantDocuments`，因此 `builder.WithRetriever` 接不上。
- **证据**：`contrib:retrieval/rag.go:77`；`goagent:interfaces/retrieval.go:8`。
- **影响**：规格原本选的"RAG 检索器"不能用，失去了它的 `IncludeMetadata`、`MaxContentLength`。
- **产品处理**：`internal/docs/index.go` 返回 `retrieval.NewVectorStoreRetriever`，来源已写进正文作为补偿。
- **建议**：修框架。给 RAGRetriever 补 `GetRelevantDocuments`，或在文档里写明它不是 Builder 检索器。

## F12 Gemini Embed 拒绝空文本

- **现象**：`GeminiProvider.Embed` 对空或纯空白文本返回 `CodeInvalidInput`。
- **证据**：Task 3 实测；`internal/web/server.go` 的 `sources` 处理函数。
- **影响**：任何把用户输入直接拿去嵌入的路径都要先判空，否则返回 500。
- **产品处理**：`/api/sources` 空查询直接返回 `[]`；哈希嵌入器对空文本返回非零向量，离线测试覆盖不到这一差异。
- **建议**：已知限制，在 provider 文档里写明。

## F13 每个 Qdrant store 自建 gRPC 客户端，检索器无法关闭

- **现象**：`NewQdrantVectorStore` 每次都新建 gRPC 客户端，不能注入现有客户端；`VectorStoreRetriever` 不暴露 Close。
- **证据**：`contrib:retrieval/qdrant/vector_store_qdrant.go:101`。
- **影响**：如果每次构建 Agent 都新建检索器，连接会持续泄漏。
- **产品处理**：`internal/docs/index.go` 在 Index 内懒创建并复用一个 alias 绑定的 store，`Index.Close` 时关闭（`TestRetrieverReusesStore`）。
- **建议**：修框架。Config 接受外部 `*qdrant.Client`。

## F14 Qdrant go-client v1.16.0 与服务端 1.20.2 不匹配，每次连接打 WARN

- **现象**：每次建立连接都输出一条客户端与服务端版本不兼容的 WARN。compose 日志、测试日志里都会出现。
- **证据**：`go.mod` 的 `github.com/qdrant/go-client v1.16.0`（随 `retrieval/qdrant v0.4.1`）；`docker compose logs` 里每个进程启动时都有。
- **影响**：日志噪声，也会掩盖真正的告警。
- **产品处理**：无。
- **建议**：修框架。contrib 升级 go-client，或关闭版本检查。

## F15 pgstore 的 tag 满足不了 management 的依赖

- **现象**：management@396d3ec 要求 `durable/pgstore v0.5.1-0.20261107151427-7637a02d2a30`，比 v0.5.0 新，所以 MVS 不会降到 v0.5.0；而 7637a02 与 goagent v0.54.1 编译不过（`session.TypeBoundarySnapshot` 未定义）。
- **证据**：`go.mod` 把 pgstore 钉在 master 伪版本 `v0.5.1-0.20261116130024-396d3eca4884`（台账裁决）。
- **影响**：同时使用 management 和 pgstore 的宿主拿不到任何能用的 tag 组合。
- **产品处理**：钉伪版本。
- **建议**：修框架。发 `durable/pgstore/v0.5.1`，management 依赖它并打 tag。

## F16 请求分隔符未导出，召回块排在请求之后

- **现象**：Builder 把证据和请求拼成 `evidence + "\n用户请求：" + input`，但这个分隔符没有导出；长期记忆的召回块作为请求之后的一条独立 user 消息追加。
- **证据**：`goagent:builder/tool_events.go:50`；`goagent:memory/recall.go:18`（`RecallPrefix`）。
- **影响**：依赖"最后一条 user 消息就是请求"的模型适配、中间件、日志或缓存键分析会取错内容，宿主只能硬编码这个分隔符。
- **产品处理**：离线脚本模型跳过召回块，并按最后一个 `"\n用户请求："` 切出请求（`internal/chat/script.go:37`、`:72`）。真实模型不受影响。
- **建议**：修框架。导出分隔常量，或把召回块放在请求之前或放进 system 消息。

## F17 工具的 `in.ExecutionKey` 无法映射回所属 Task

- **现象**：工具收到的 `ExecutionKey` 是 `durable.ChildKey(父键, "tool/"+CallID)` 的摘要，宿主无法从它反查任务。
- **证据**：`goagent:core/durable/operation.go:45`；`internal/assistant/agent.go:70-71` 记录的 execution_key 无法与 `mg.Task` 关联。
- **影响**：要按任务统计工具执行次数（"审批后恰好执行一次"），只能另想办法。
- **产品处理**：e2e 每个场景用随机租户，按"租户 + 工具名"计数（`TestAcceptance/TestApprovalSurvivesKillAndFailover`）。
- **建议**：修框架。在 `ToolInput` 或 `tools.Call` 上提供任务 ID 或根执行键。

## F18 management 不保留执行错误原文

- **现象**：`Runtime.Run` 返回普通错误时，任务记为 `failed`，`ErrorKind="execution"`，但错误文本既不写入 Task，也不经 `Config.Report` 报告。
- **证据**：`goagent:management/worker.go:769-771`。
- **影响**：线上任务失败时，运维从管理面和日志里都看不到原因。开发期间出现过两次 `failed(execution)`，因为拿不到错误原文，至今无法定位。
- **产品处理**：无，不在 Runtime 外层另造一套错误记录。
- **建议**：修框架。把脱敏后的错误摘要写入 Attempt，或经 Report 输出。

## F19 Worker.Run 在认领槽位途中被取消会泄漏槽位（框架缺陷）

- **现象**：`Worker.Run` 用可取消的 ctx 认领槽位，而 pgx 的 `BeginFunc` 用同一个 ctx 提交事务。取消如果落在提交前后，客户端拿到 `context canceled` 后直接返回，不会执行释放；服务端却可能已经提交了认领。结果是槽位归属一个已经退出的 Worker，之后的实例都报"执行宿主已被占用"。
- **证据**：
  - `goagent:management/worker.go:139-146`（`ctx, cancel := context.WithCancel(parent)` 之后 `w.owner(ctx, false)`）。
  - 临时复现测试中，启动后 0–3.5ms 内取消，40 次里泄漏 15 次。
  - Task 7 冒烟时，`ka serve` 输出"已启动"后立即发 SIGTERM，泄漏复现。
- **影响**：
  - 滚动发布或编排系统在启动几毫秒内发出 SIGTERM，都会让 Worker 永久失联，直到人工回收。
  - 更隐蔽的是，`cmd/ka` 把 `context.Canceled` 当作正常退出，这种情况下进程以 0 退出，泄漏被掩盖。
- **产品处理**：
  - 不在产品里绕过。`ka recover-worker` 用于事后回收。
  - `scripts/failover.sh` 以 WORKER=1 重建 b 后，会跑完一个探测任务才返回，确认槽位已经持有，期间不发信号。
  - e2e 也先跑探测任务，再对 Worker 进程发信号。
- **建议**：修框架。认领改用 `context.WithoutCancel` 加超时；或者认领返回错误时，按本 Worker id 尝试释放一次。

## F20 任意租户的 Actor 都能回收 Worker 槽位

- **现象**：`TenantAuthorizer` 对 `worker.recover` 只校验 `ref.TenantID == a.TenantID`，而 `auth()` 对 workers 资源用的是 Actor 自己的租户，因此任何租户都能通过。
- **证据**：`goagent:management/worker.go:103`；`goagent:management/types.go:304`。
- **影响**：当前只有 Go API 暴露回收，httpapi 没有路由，所以没有实际风险。但内置授权器对系统级操作不设防，宿主一旦经 HTTP 暴露，任何用户都能摘掉正在运行的 Worker。本产品的 Bearer 方案也允许用户名 `ops`。
- **产品处理**：只经 CLI 以固定的 ops Actor 调用。
- **建议**：修框架。系统级操作要求显式的运维角色；至少在文档里写明。

## F21 `memory.Scored` 的分数字段没有 json tag

- **现象**：`Score`、`Relevance` 序列化后是大写键，与 `Record` 的小写字段风格不一致。
- **证据**：`goagent:memory/memory.go:430-434`。
- **影响**：直接输出召回结果的 API 形状不统一。
- **产品处理**：`/api/memories` 原样输出，页面不展示分数。
- **建议**：修框架，加 `json:"score"`、`json:"relevance"`。

## F22 模型客户端未声明缓存身份时，模型缓存静默失效

- **现象**：`clientIdentity` 依次尝试客户端的 `CacheIdentity()`、请求的 `Model`、客户端的 `ModelName()`。三者都取不到时，这次调用不缓存，也不告警。Builder 发出的请求不带 Model。
- **证据**：
  - `goagent:builder/model_cache_key.go:105-118`、`goagent:builder/model_cache.go:237-239`。
  - e2e `TestAcceptance/TestForgetAcrossInstancesErasesCache` 首轮失败：提问后 Redis 里没有新增任何键。
- **影响**：本产品配置了 `EraseRequired`，Build 也全部通过，但缓存从未生效。"缓存擦除"的验收如果只断言"删除后不存在"，会空洞地通过。
- **产品处理**：提交 dad6454 修复，脚本模型声明 `CacheIdentity`，真实模型客户端声明 `ModelName`（`internal/chat/script.go:26`、`internal/chat/livemodel.go:111`）。真实模型下缓存生效的证据见验证状态一节。
- **建议**：修框架。配置了 ModelCache 而客户端无法解析身份时，在 Build/Validate 阶段报错，或至少告警一次。

## F23 cache/redis 的会话标签键在遗忘后保留到过期

- **现象**：一次提问生成的缓存条目同时挂着记忆标签和会话标签。删除记忆时，条目（`e:`）被物理删除，但会话标签的令牌（`g:`）和成员集合（`t:`）保留到 TTL。
- **证据**：`contrib:cache/redis/tagged.go:134-140`（`DeleteTag` 只删被删标签自己的令牌与成员）；e2e 断言放宽为"`e:` 全部不存在，`g:`/`t:` 必须带 TTL"（台账裁决）。
- **影响**：这些键只含 HMAC 摘要和缓存键，不含内容，按协议属预期行为。但如果合规要求"派生数据全部立即消失"，现有协议做不到。
- **产品处理**：无。
- **建议**：写明语义。如需立即删除，向 cache/redis 提出新的协议需求。

## F24 运维命令也须装配完整 Factory

- **现象**：`mg.New` 要求 `Factories` 非空，所以只调用 `RecoverWorkerSlot` 的运维命令，也要装配 Agent 所需的全部依赖。
- **证据**：`goagent:management/service.go:20`；`cmd/ka/main.go` 的 `recoverWorker` 复用 serve 的 `open`。
- **影响**：故障转移时也要连 Qdrant、Redis，并探测一次 Gemini 嵌入。外部服务本身故障时，回收槽位也会跟着失败。
- **产品处理**：复用完整装配。
- **建议**：修框架。提供只依赖 Store 的运维入口，例如 `mg.NewAdmin(store, authorizer)`。

---

## 产品侧已知限制（不属于框架）

- `docs.Collect` 遇到一个无法解析的 `.go` 文件就整体失败。当前两个固定版本的语料都能解析。
- 并发执行 `ka ingest` 没有互斥，按"单写者 CLI"设计；运行中的重建只经审批由唯一的 Worker 执行。
- e2e 用了 `Pdeathsig`，只能在 Linux 上编译。
- 页面 JS 没有自动化测试。

## 验证状态

验证于 2026-11-16（UTC），产品提交 5343471，框架为 goagent v0.54.1，contrib 为 396d3eca4884。

- **离线**：`TMPDIR=/tmp taskset -c 2,3,6-27 scripts/test.sh ./...` 全部通过（e2e、assistant、chat、config、docs、embed、web）。
- **compose 演练**（`KA_MODEL=script KA_EMBEDDER=hash`）：ingest 索引 5562 块。a 持有 Worker 槽位，b 不持有。`scripts/failover.sh` 退出码 0，用时约 12 秒，回收了 a 的槽位（代次 1）。之后 b 以 `worker=true` 重启，探测任务成功。此次演练早于下面的语料修正，所以块数更多。
- **真实模型首次验收失败**：报告见 `verification/live-20261116T165540Z.json`，提交 9927f15，模型 deepseek，嵌入 gemini，失败 2 项。两个记忆场景都通过（a 写入、b 召回、经 b 删除后 a 不再召回，召回产生的 `e:` 缓存条目被物理删除）。两项失败都是产品问题，不是框架问题：
  - 语料收录了 `docs/superpowers` 下带日期的设计和实施计划，真实嵌入下这些计划排在模块文档前面。「Redis 缓存前缀默认是什么」的答案因此来自旧计划。
  - godoc 采集漏掉了导出结构体字段的注释，而默认前缀只写在 `Config.Prefix` 的字段注释里。
  - 验收脚本只按 `dir/` 前缀判断来源，`repo:dir#godoc` 这种只有包文档的来源会被误判为不匹配。
- **修复**：提交 5343471 排除 `docs/superpowers`，并收录导出字段注释，均先写失败测试；验收脚本的来源判断改为同时接受 `dir#`。
- **真实模型复验通过**：`KA_LIVE=1 scripts/live.sh` 退出码 0，报告见 `verification/live-20261116T170741Z.json`，失败 0 项。ingest 索引 4233 块。
  - 3 个文档问题的来源依次为：`durable/pgstore#godoc` 与 contrib `README.md#durable/pgstore`；`CACHING_GUIDE.md#删除承诺`；`cache/redis#godoc`（答案 `goagent:cache:`）。耗时 2.3–9.3 秒，每题约 2400–2900 tokens，`usage_status=available`。
  - 记忆场景：a 写入后 b 可见 1 条；b 新会话召回成功，新增 2 个 `e:` 缓存条目；经 b 删除后 a 查不到记录，这些缓存条目不再存在，a 新会话回答中不再出现代号。
- **已知局限**：`sources_of` 按正则从回答里提取来源。它会把正文里的字面量（如 `goagent:cache:`）和紧跟的全角括号说明也算进来，所以报告中的 `sources_in_answer` 有噪声。判定只要求有一个来源命中期望前缀。期望前缀较宽时（第 2 题的 `goagent:`），正文里的字面量也可能命中。本次第 2 题的两个来源都是真实文档小节，不受影响。收紧提取规则是后续工作。

## 后续工作

- **文档向量改存 Postgres（规格方案 B）**：在 contrib 新增 pgvector 版 `interfaces.VectorStore`。届时部署只需 Postgres + Redis，同时补上 F5 的 alias/维度问题。
- 修复上面标"修框架"的条目后，删除产品里对应的包装：F1 nsRuntime、F2 直读槽位文档、F13 store 复用、F16 脚本模型的硬编码分隔符。
