# 客服编排测试页面 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 customer-service 加 `serve` 命令。它常驻一个宿主，提供用户页 `/customer` 与客服台 `/desk`，用来在浏览器里手动测试客服流程与长期记忆。

**Architecture:**
- **账本**：升到 v2，给 `ticket_messages`、`outbox` 加写入时间，用于合并排序。
- **读模型与人工回复**：宿主新增对话、工单列表、Agent 处理过程三个读模型，以及人工回复，都放在 `console.go`。
- **HTTP 层**：`web.go` 只用标准库 `net/http`，把耗时的写操作放到后台协程，用一把锁串行。
- **页面**：`go:embed` 内嵌纯 HTML/JS 页面。

**Tech Stack:** Go 1.27、标准库 `net/http`、`net/http/httptest`、`embed`、SQLite（现有 `mattn/go-sqlite3`）、testify。

**Spec:** `docs/superpowers/specs/2026-11-17-customer-service-web-design.md`

## Global Constraints

- **工作区**：examples worktree `../goagent-examples-cs-mem`，分支 `feat/cs-memory`。用现有 `go.work`（`make work CONTRIB=...` 生成，已 gitignore），不发版，不改 `go.mod`。
- **依赖**：只用标准库与现有依赖，不引入前端构建链或新 Go 依赖。
- **监听地址**：`serve` 默认 `-addr 127.0.0.1:8090`。
- **提交人**：页面提交人固定为 `agent-1`。
- **刷新**：页面每 2 秒刷新一次。
- **命名空间**：Agent 运行时的命名空间只取工单行的 CustomerID（现有 `step`，不改）。记忆接口的命名空间取路径中的客户 ID，经 `Host.Memories` / `Host.MemoryRecord`。
- **错误格式**：错误响应统一为 `{"error":{"code":"…","message":"…"}}`。消息用中文，经 `livemodel.Redact`。任何响应与日志不输出 API key 或连接串。
- **未开启 `-memory`**：现有 CLI 行为与测试不变（除账本 v2 带来的新目录要求），且不需要容器。
- **人工回复**：
  - 幂等键 `<ticketID>/manual/<replyID>`，kind `support.KindCustomerReply`，标题"人工客服"，`channel=notify`。
  - "转过人工" = 发件箱存在该工单 `channel=handoff` 的记录。
  - `needs_human` = 转过人工，且该工单最后一条 handoff 记录（按 `outbox.seq`）之后没有人工回复。
- **写入方式**：发消息和审核异步执行，返回 202；人工回复和 sweep 同步执行。四者共用一把 `sync.Mutex`，读接口不加锁。
- **语言**：注释、错误信息、提交信息用简体中文。
- **测试命令**：
  - 前缀 `TMPDIR=/tmp taskset -c 2,3,6-27`，在 examples worktree 根目录运行，`-p 1`。
  - 需要容器的测试：`(cd orchestration/customer-service && scripts/with-stores.sh go test -p 1 -run <名> .)`。
  - 等待异步结果用 `require.Eventually`，不用固定 `time.Sleep`。

## 相对规格的两处更正（随本计划一并改进规格）

- **`created_at` 取 SQLite 写入时的系统时间**：`strftime('%Y-%m-%dT%H:%M:%fZ','now')` 作为列默认值，不取宿主时钟。测试与 CLI `-now` 用固定时钟，所有记录时间相同，无法排序；这一列只用于显示排序。
- **人工回复接口去掉 `staff`**：现有 `support.Message` 无处存放该字段，标题固定为"人工客服"。

## Review Focus

1. **页面重试同一 `message_id`**（网络超时后重发）：只出现一条客户消息和一条回复。测试在 Task 3。
2. **路径里的客户 ID 含 `/`、`..`、空格，或文本为空、超长**：返回 400，不触及宿主。测试在 Task 3。
3. **上一条消息还在处理时客户又发一条**：两条都被处理，按发送顺序显示，第二条成为排队消息或新工单。测试在 Task 3。
4. **人工回复不存在的工单**：返回 404，不写发件箱。测试在 Task 2（宿主层）与 Task 3（HTTP 层）。
5. **按 ID 查看另一个客户的记忆记录**：返回 404，读不到。测试在 Task 4。

---

### Task 1: 账本 v2 与客户对话读模型

**Files:**
- Modify: `orchestration/customer-service/ledger.go`。`schemaVersion` 改为 2，建表语句给 `ticket_messages`、`outbox` 末尾加 `created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))`。
- Modify: `orchestration/customer-service/host.go:308`。`INSERT INTO ticket_messages` 改为显式列名 `(message_id, ticket_id, seq, text, handled)`。
- Modify: `orchestration/customer-service/sweeper_test.go:414,448`。两处位置式插入同样改为显式列名，测试意图不变。
- Create: `orchestration/customer-service/console.go`
- Test: `orchestration/customer-service/console_test.go`

**Interfaces:**
- Produces:
  ```go
  // ConversationEntry 是对话中的一条：客户消息或发给客户的通知。
  type ConversationEntry struct {
      TicketID  string `json:"ticket_id"`
      Role      string `json:"role"`                 // "customer" 或 "service"
      MessageID string `json:"message_id,omitempty"` // 仅客户消息
      Kind      string `json:"kind,omitempty"`       // 仅通知：support.MessageKind
      Title     string `json:"title,omitempty"`
      Text      string `json:"text"`
      At        string `json:"at"`               // created_at
      Status    string `json:"status,omitempty"` // 仅通知：outbox.status
      Manual    bool   `json:"manual,omitempty"` // 幂等键含 "/manual/"
  }
  func (h *Host) Conversation(ctx context.Context, customerID string) ([]ConversationEntry, error)
  ```
  - 范围：该客户全部工单的 `ticket_messages`，加上 outbox 中 `channel='notify'` 且 recipient（`support.User` 无 json 标签，编码为 `{"ID":…,"Name":…}`）的 ID 等于该客户的记录。
  - 排序：`At` 升序；时间相同时客户消息在前；同角色按各自表的 `seq` 升序。
  - 空结果返回非 nil 空切片。

- [ ] **Step 1: 写失败的测试** `console_test.go`，表驱动，用 `newHostEnv(t, nil)`：
  ```go
  func TestConversation(t *testing.T) {
      // "退款与回复交错"：e.message("m1","c-1001","订单 A1001 申请退款 30 元") 后 Conversation("c-1001")
      //   第 1 条 Role=="customer"、MessageID=="m1"；之后至少一条 Role=="service"；所有 At 非空且不降序。
      // "多轮第一轮不丢"：同一工单在运行中排队第二条消息（复用 host_test 中已有的排队用例输入）后，
      //   两条客户消息都在，且顺序为 m1 在 m2 前。
      // "转人工只见告知"：e.message("m1","c-1001","乱码乱码")（分流无效转人工）后，
      //   service 条目的 Kind 只含 "handoff_notice"/"customer_reply"/"progress" 等通知 kind；
      //   不存在 Text 含 handoff 摘要的条目（断言没有任何条目 Text 等于 e.handoff.Transfers()[0].Context.Summary）。
      // "隔离"：c-1001 与 c-2002 各发一条后，Conversation("c-2002") 的所有条目 TicketID 均属于 c-2002 的工单，
      //   且不含审核人 fin-1 的通知。
      // "无数据"：Conversation("c-none") 返回 len==0 且非 nil。
  }
  func TestLedgerV1Rejected(t *testing.T) {
      // 打开一次宿主后关闭，用 sqlite 直接把账本 PRAGMA user_version 设为 1，再 tryOpen()：
      // require.ErrorContains(err, "账本结构不兼容")
  }
  ```
- [ ] **Step 2: 运行确认失败**：`TMPDIR=/tmp taskset -c 2,3,6-27 go test -p 1 -run 'TestConversation|TestLedgerV1Rejected' ./orchestration/customer-service/`，预期编译失败（`Conversation` 未定义）。
- [ ] **Step 3: 实现**：改 `ledger.go`、`host.go:308` 与 `sweeper_test.go` 两处插入，在 `console.go` 实现 `Conversation`。账本文件路径从 `ledger.go` 现有打开逻辑取，测试按同一路径打开。
- [ ] **Step 4: 运行通过**：同 Step 2；再跑 `TMPDIR=/tmp taskset -c 2,3,6-27 go test -p 1 ./orchestration/customer-service/`，预期全部 PASS（记忆相关测试无环境变量时 SKIP）。
- [ ] **Step 5: 提交**
  ```bash
  git add orchestration/customer-service/ledger.go orchestration/customer-service/host.go orchestration/customer-service/sweeper_test.go orchestration/customer-service/console.go orchestration/customer-service/console_test.go
  git commit -m "feat(customer-service): 账本记录写入时间，宿主提供客户对话视图"
  ```

### Task 2: 工单列表、Agent 处理过程与人工回复

**Files:**
- Modify: `orchestration/customer-service/console.go`
- Modify: `orchestration/customer-service/host.go:194-200`。`ticket` 在找不到时返回 `ticketNotFoundError`，`Error()` 文本保持 "工单 %s 不存在"。
- Test: `orchestration/customer-service/console_test.go`

**Interfaces:**
- Consumes: Task 1 的 `console.go`、账本 v2。
- Produces:
  ```go
  type ticketNotFoundError struct{ id string }   // Error(): "工单 <id> 不存在"
  var errNotHandedOff = errors.New("工单未转人工，不能人工回复")

  type TicketSummary struct {
      TicketID     string `json:"ticket_id"`
      CustomerID   string `json:"customer_id"`
      Status       string `json:"status"`
      Round        int    `json:"round"`
      LastProgress string `json:"last_progress"`
      NeedsHuman   bool   `json:"needs_human"`
  }
  // Tickets 按 last_progress 降序；customerID 为空时返回全部。空结果为非 nil 空切片。
  func (h *Host) Tickets(ctx context.Context, customerID string) ([]TicketSummary, error)

  // AgentTurns 读取工单当前运行检查点中的 Ticket.Turns（h.graph.Snapshot，读超时 h.cfg.LockTimeout）；
  // 运行尚无检查点时返回空切片，工单不存在返回 ticketNotFoundError。
  func (h *Host) AgentTurns(ctx context.Context, ticketID string) ([]multiagent.Turn, error)

  type ReplyResult struct {
      TicketID string `json:"ticket_id"`
      Key      string `json:"key"`
  }
  // Reply 写入一条人工回复并投递（h.deliver）；同一 replyID 重复调用不重复写入。
  // 工单不存在 → ticketNotFoundError；未转过人工 → errNotHandedOff；replyID 或 text 为空 → 普通错误。
  func (h *Host) Reply(ctx context.Context, ticketID, replyID, text string) (ReplyResult, error)
  ```

- [ ] **Step 1: 写失败的测试**（追加到 `console_test.go`，表驱动）：
  ```go
  func TestReply(t *testing.T) {
      // "转人工后可回复"：e.message("m1","c-1001","乱码乱码")，状态已为 delivered；
      //   Tickets("c-1001")[0].NeedsHuman == true；
      //   Reply(tk,"r1","您好，我是人工客服") 无错误，Key == tk+"/manual/r1"；
      //   e.sent(support.KindCustomerReply) 中有一条 Title=="人工客服"、Body 为回复内容；
      //   Conversation 末条 Manual==true；Tickets 中 NeedsHuman == false。
      // "重复提交只投递一次"：同一 replyID 再调用一次无错误，Title=="人工客服" 的投递仍为 1 条。
      // "未转人工拒绝"：正常完成的工单（如 TestHostHandoff 之外的普通咨询输入）Reply → errors.Is(err, errNotHandedOff)；发件箱条数不变。
      // "工单不存在"：Reply("tk-none",…) → errors.As(err, &ticketNotFoundError{})；发件箱条数不变。
      // "运行失败也算转人工"：复用 host_test 中使运行失败、写 handoff 的现有配置（如 MaxModelRequests 触发的路径），NeedsHuman==true。
  }
  func TestTicketsAndTurns(t *testing.T) {
      // c-1001、c-2002 各一张工单：Tickets("") 长度 2；Tickets("c-2002") 只有 c-2002 的。
      // AgentTurns(退款工单) 非空，且含 Agent == 分流成员名（取 graph.go 中的常量）的一轮。
      // AgentTurns("tk-none") → ticketNotFoundError。
  }
  ```
- [ ] **Step 2: 运行确认失败**：`TMPDIR=/tmp taskset -c 2,3,6-27 go test -p 1 -run 'TestReply|TestTicketsAndTurns' ./orchestration/customer-service/`，预期编译失败。
- [ ] **Step 3: 实现上述签名**：`Reply` 在 `h.write` 事务内校验并调用现有 `notify`，事务后调用 `h.deliver(ctx)`。`needs_human` 用一条 SQL 按 `outbox.seq` 判断，不在 Go 里逐条扫描。
- [ ] **Step 4: 运行通过**：同 Step 2，再跑整个包，预期 PASS。
- [ ] **Step 5: 提交**
  ```bash
  git add orchestration/customer-service/console.go orchestration/customer-service/console_test.go orchestration/customer-service/host.go
  git commit -m "feat(customer-service): 工单列表、Agent 处理过程与转人工后的人工回复"
  ```

### Task 3: HTTP 接口与后台执行

**Files:**
- Create: `orchestration/customer-service/web.go`
- Test: `orchestration/customer-service/web_test.go`

**Interfaces:**
- Consumes: Task 1、2 的宿主方法；现有 `Host.Message`、`Host.Answer`、`Host.Sweep`、`Host.Show`、`Host.Memories`、`Host.MemoryRecord`；`h.memory == nil` 表示未开启记忆。
- Produces:
  ```go
  // webServer 持有宿主、写锁与后台任务；base 是后台任务的上下文，Close 时在等待超时后取消。
  type webServer struct { /* host *Host; mu sync.Mutex; jobs map[string]jobState; … */ }
  func newWebServer(h *Host) *webServer
  func (s *webServer) Handler() http.Handler   // 含 /api/* 与 Task 4 的页面路由
  func (s *webServer) Close(wait time.Duration) // 等后台任务结束，超时则取消 base 再等其退出
  ```
- 路由（Go 1.22+ 模式匹配）：
  | 路由 | 行为 | 成功响应 |
  |---|---|---|
  | `POST /api/customers/{c}/messages` `{message_id,text}` | 后台持锁 `Message(c,"agent-1",id,text)`，任务键 `message:<id>` | 202 `{"message_id"}` |
  | `GET /api/customers/{c}/conversation` | `Conversation` + 该客户未完成或出错的后台任务 | 200 `{"entries":[…],"pending":[{"message_id","text","error"}]}` |
  | `GET /api/tickets?customer=` | `Tickets` | 200 `{"tickets":[…]}` |
  | `GET /api/tickets/{t}` | `Show` + `AgentTurns` + 任务 `answer:<t>` 的状态 | 200 `{"ticket":TicketView,"turns":[…],"answer":{"running","error"}}` |
  | `POST /api/tickets/{t}/answer` `{approver,approve,note}` | 先 `Show` 确认工单存在（404），后台持锁 `Answer` | 202 |
  | `POST /api/tickets/{t}/reply` `{reply_id,text}` | 同步持锁 `Reply` | 200 `ReplyResult` |
  | `POST /api/sweep` | 同步持锁 `Sweep` | 200 `SweepReport` |
  | `GET /api/customers/{c}/memories?q=` | `Memories` | 200 `MemoryView` |
  | `GET /api/customers/{c}/memories/{id}` | `MemoryRecord` | 200 `memory.Record` |
- 错误码表（声明为一张表，由一个函数把错误映射为状态码）：
  | code | 状态 | 条件 |
  |---|---|---|
  | `bad_request` | 400 | 客户 ID 不匹配 `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`；`message_id`/`reply_id` 不匹配同一正则；`text` 为空或超过 2000 个字符（rune）；请求体不是 JSON 或超过 64 KiB |
  | `not_found` | 404 | `ticketNotFoundError`；记忆记录不存在（`MemoryRecord` 返回的找不到错误，实现者查 goagent `memory` 包的哨兵错误） |
  | `conflict` | 409 | `errNotHandedOff` |
  | `memory_disabled` | 400 | 未开启 `-memory` 时访问记忆接口，消息"未开启长期记忆（启动 serve 时加 -memory）" |
  | `internal` | 500 | 其他；消息为 `livemodel.Redact(err.Error())` |
- 后台任务：
  - 成功后从 `jobs` 删除；失败保留，错误经 Redact 后存入。
  - `pending` 列表只含该客户的 `message:` 任务。页面按 `message_id` 与已出现的客户消息去重。

- [ ] **Step 1: 写失败的测试** `web_test.go`，用 `newHostEnv(t, nil)` 加 `httptest.NewServer(newWebServer(e.host).Handler())`。辅助函数 `postJSON`、`getJSON`，以及 `waitConversation(t, srv, c, func([]ConversationEntry) bool)`（`require.Eventually` 5s/20ms）。表驱动用例：
  ```go
  func TestWebMessages(t *testing.T) {
      // "发消息后看到回复"：POST m1 → 202；等到 entries 含 Role=="service"；pending 为空。
      // "重试同一 message_id"：POST m1 两次 → 两次 202；最终 Role=="customer" 的条目只有 1 条，
      //   KindCustomerReply 投递数与只发一次时相同。
      // "连续两条"：不等待地 POST m1、m2 → 最终两条客户消息都在，m1 在 m2 前。
      // "非法输入"：客户 ID "a/b"（URL 编码）、"../x"、"a b"；text ""；text 2001 个"字"；message_id "" →
      //   400，code=="bad_request"；e.host 的 tickets 表行数为 0。
      // "后台出错可见"：让 Message 必然失败的输入（如 mutate 使 Store 返回错误的现有 faultStore 开关）
      //   → pending 中该 message_id 的 error 非空。
  }
  func TestWebDesk(t *testing.T) {
      // "审核批准"：退款进入 waiting_review 后 POST answer {approver:"fin-1",approve:true} → 202；
      //   等到 GET /api/tickets/{t} 的 ticket.status 不再是 waiting_review。
      // "审核不存在的工单"：→ 404 not_found。
      // "人工回复"：转人工工单 POST reply {reply_id:"r1",text:"…"} → 200；再次同一 reply_id → 200 且投递仍 1 条；
      //   未转人工工单 → 409 conflict；"tk-none" → 404。
      // "工单列表"：GET /api/tickets?customer=c-1001 → 只含 c-1001，转人工工单 needs_human==true。
      // "sweep"：POST /api/sweep → 200 且可解码为 SweepReport。
      // "未开启记忆"：GET /api/customers/c-1001/memories → 400 code=="memory_disabled"。
  }
  func TestWebIsolation(t *testing.T) {
      // c-1001、c-2002 各发一条：c-2002 的 conversation 中所有 ticket_id 都属于 c-2002，pending 不含 c-1001 的消息。
  }
  ```
- [ ] **Step 2: 运行确认失败**：`TMPDIR=/tmp taskset -c 2,3,6-27 go test -p 1 -run 'TestWeb' ./orchestration/customer-service/`，预期编译失败。
- [ ] **Step 3: 实现 `web.go`**：参照 `products/knowledge-assistant/internal/web/server.go` 的 `ServeMux` 路由与 JSON 写法。后台任务用 `sync.WaitGroup` 计数；写锁在协程内获取，请求处理函数不持锁等待。
- [ ] **Step 4: 运行通过**：同 Step 2，再加 `-race` 跑一次 `-run TestWeb`，再跑整个包，预期 PASS 且无数据竞争。
- [ ] **Step 5: 提交**
  ```bash
  git add orchestration/customer-service/web.go orchestration/customer-service/web_test.go
  git commit -m "feat(customer-service): 测试页面的 HTTP 接口，模型运行放到后台串行执行"
  ```

### Task 4: 页面、serve 命令、记忆接口测试与 README

**Files:**
- Create: `orchestration/customer-service/web/customer.html`、`web/desk.html`、`web/app.js`
- Modify: `orchestration/customer-service/web.go`。`//go:embed web` 挂载 `GET /customer`、`GET /desk`、`GET /web/app.js`，`GET /{$}` 302 到 `/customer`。
- Modify: `orchestration/customer-service/main.go`。`usage` 加一行 `serve   [-addr 127.0.0.1:8090]   启动测试页面（用户页 /customer，客服台 /desk）`；新增 `-addr` 选项；`serve` 分支。
- Modify: `orchestration/customer-service/README.md`
- Test: `orchestration/customer-service/web_test.go`、`orchestration/customer-service/cli_test.go`

**Interfaces:**
- Consumes: Task 3 的 `newWebServer`、`Handler`、`Close`。
- Produces:
  - `serve` 命令：用与其他命令相同的 `HostConfig`（`printer{w}` 为通知器），`http.Server{Addr: *addr}` 监听。`ctx` 结束后先 `Shutdown`（5 秒），再 `Close(cfg.LockTimeout)`，最后关闭宿主。
  - 启动后向 `w` 打印一行 JSON：`{"event":"serve","customer":"http://<addr>/customer","desk":"http://<addr>/desk"}`。
- 页面内容：
  - `/customer`：客户下拉含 `c-1001`、`c-2002`，可输入任意 ID；对话气泡按 `entries` 排列，`pending` 显示"处理中"或错误；输入框加发送按钮。`message_id` 由页面生成（`crypto.randomUUID()` 去掉 `-`），失败重试时复用。
  - `/desk`：左列为工单列表（`needs_human` 标"待人工"）和 sweep 按钮。中列为工单详情、Agent turns、审核区与人工回复区：审核区仅在 `waiting_review` 时显示，审核人下拉取 `ticket.approvers`；人工回复区仅在 `needs_human` 或转过人工时显示。右列是记忆面板：钉住、召回、按 ID 查看（显示 `superseded_by`、`invalid_at`、`pinned`），客户跟随所选工单。
  - 每 2 秒刷新当前视图，用 `textContent` 渲染，不用 `innerHTML` 拼接数据。

- [ ] **Step 1: 写失败的测试**：
  ```go
  func TestWebPages(t *testing.T) {
      // GET /customer → 200，Content-Type 含 text/html，正文含 "客户"；
      // GET /desk → 200，正文含 "客服台"；GET /web/app.js → 200；GET / → 302 Location "/customer"。
  }
  func TestWebMemory(t *testing.T) { // 用 newMemoryEnv，缺环境变量时 Skip
      // c, _ := newCustomer()；POST messages {message_id: msgID 去掉 "-" 后, text:"请记住：只用邮件联系我"}；
      // 等到出现 service 条目；GET memories → pinned 长度 1，内容含 "邮件"（不需要 sweep）；
      // GET memories/{pinned[0].ID} → 200；
      // 另一个客户 c2 的 GET memories/{同一 ID} → 404 not_found；
      // POST /api/sweep → memory_jobs > 0。
  }
  ```
  在 `cli_test.go` 的现有用法测试表中加一例：`serve` 的 `-addr` 被占用时（先用 `net.Listen` 占住 `127.0.0.1:0` 拿到地址）`execute` 返回错误，不挂起。
- [ ] **Step 2: 运行确认失败**：`TMPDIR=/tmp taskset -c 2,3,6-27 go test -p 1 -run 'TestWebPages|TestCLI' ./orchestration/customer-service/`，预期 FAIL。
- [ ] **Step 3: 实现**页面、路由与 `serve` 分支。
- [ ] **Step 4: README**：在"长期记忆"一节之后加"测试页面"一节，内容包括：
  - 离线和 `-memory -live` 两种启动命令，记忆的启动命令经 `scripts/with-stores.sh`；
  - 两个页面各能做什么；
  - 人工回复不进入记忆；
  - 账本 v2 需要新的 `-dir`；
  - 页面没有鉴权，只监听本机；
  - serve 运行时同一 `-dir` 不要再运行 CLI 写命令。
- [ ] **Step 5: 运行通过**：
  - 整个包：`TMPDIR=/tmp taskset -c 2,3,6-27 go test -p 1 ./orchestration/customer-service/`
  - 记忆测试：`(cd orchestration/customer-service && TMPDIR=/tmp taskset -c 2,3,6-27 scripts/with-stores.sh go test -p 1 -run 'TestWebMemory|TestMemory' .)`
  - 预期全部 PASS，`TestWebMemory` 不是 SKIP。
- [ ] **Step 6: 冒烟**：
  - 后台启动 `go run ./orchestration/customer-service serve -dir /tmp/cs-web-smoke`。
  - `curl -s localhost:8090/customer | head -1` 有 HTML。
  - `curl -s -XPOST localhost:8090/api/customers/c-1001/messages -d '{"message_id":"s1","text":"订单 A1001 申请退款 30 元"}'` 返回 202。
  - 2 秒后 conversation 含 service 条目。
  - 停掉进程，删除 `/tmp/cs-web-smoke`。
- [ ] **Step 7: 提交**
  ```bash
  git add orchestration/customer-service/web orchestration/customer-service/web.go orchestration/customer-service/web_test.go orchestration/customer-service/main.go orchestration/customer-service/cli_test.go orchestration/customer-service/README.md
  git commit -m "feat(customer-service): serve 命令提供用户页与客服台，README 说明测试页面"
  ```

## 交付后的手动验证（控制者执行，不属于任何任务）

用浏览器跑两遍：先离线，再 `-memory -live`（经 with-stores.sh）。每遍的步骤：

1. 用户页依次发退款和"请记住：只用邮件联系我"。
2. 在客服台审核退款，执行 sweep。
3. 在记忆面板查看钉住记录和旧记录。
4. 发一条触发转人工的消息，人工回复后，在用户页确认能看到回复。
