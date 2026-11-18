package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/internal/livemodel/livemodeltest"
	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"
)

// scenarioState 是作答前读到的记忆状态，供状态检查使用。
type scenarioState struct {
	valid  []memory.Record
	pinned []memory.Record
	// chains 是 pinchain 中每个曾被钉住的记录沿 SupersededBy 走出的链。
	chains []supersedeWalk
}

// stateCheck 返回空串表示通过，否则返回失败说明。
type stateCheck func(s scenarioState) string

func anyContains(recs []memory.Record, word string) bool {
	return slices.ContainsFunc(recs, func(r memory.Record) bool { return strings.Contains(r.Content, word) })
}

func validHas(word string) stateCheck {
	return func(s scenarioState) string {
		if anyContains(s.valid, word) {
			return ""
		}
		return "有效记录中没有含“" + word + "”的"
	}
}

func validLacks(word string) stateCheck {
	return func(s scenarioState) string {
		if anyContains(s.valid, word) {
			return "有效记录中仍有含“" + word + "”的"
		}
		return ""
	}
}

// validStale 要求没有只提旧值、不提新值的有效记录。新记录写「由 old 改为 current」是正常的，只有仍声称 old 的记录才算残留。
func validStale(old, current string) stateCheck {
	return func(s scenarioState) string {
		if slices.ContainsFunc(s.valid, func(r memory.Record) bool {
			return strings.Contains(r.Content, old) && !strings.Contains(r.Content, current)
		}) {
			return "有效记录中仍有只含“" + old + "”、不含“" + current + "”的"
		}
		return ""
	}
}

func pinnedHas(word string) stateCheck {
	return func(s scenarioState) string {
		if anyContains(s.pinned, word) {
			return ""
		}
		return "钉住记录中没有含“" + word + "”的"
	}
}

// validOrPinnedHas 要求有效记录或钉住记录中含 word。
func validOrPinnedHas(word string) stateCheck {
	return func(s scenarioState) string {
		if anyContains(s.valid, word) || anyContains(s.pinned, word) {
			return ""
		}
		return "有效记录和钉住记录中都没有含“" + word + "”的"
	}
}

func pinnedLacks(word string) stateCheck {
	return func(s scenarioState) string {
		if anyContains(s.pinned, word) {
			return "钉住记录中有含“" + word + "”的"
		}
		return ""
	}
}

// chainsPinned 要求取代链上每条记录都钉住、末端有效。
func chainsPinned(s scenarioState) string {
	var bad []string
	for _, w := range s.chains {
		for i, r := range w.chain {
			if !r.Pinned {
				bad = append(bad, fmt.Sprintf("链 %s 第 %d 条 %s 未钉住", w.start, i+1, r.ID))
			}
		}
		if w.broken != "" {
			bad = append(bad, w.broken)
		} else if end := w.chain[len(w.chain)-1]; !end.Valid() {
			bad = append(bad, fmt.Sprintf("链 %s 末端 %s 已失效（%s）", w.start, end.ID, end.InvalidReason))
		}
	}
	return strings.Join(bad, "；")
}

// liveScenario 描述一个场景：prior 是提问前依次开启的工单，最后一张工单 question 的答复交给评审。
type liveScenario struct {
	name     string
	kind     judgeKind
	prior    []string
	question string
	expect   string
	checks   []stateCheck
	// invariant 为真时状态检查失败即缺陷，测试失败。
	invariant bool
	// isolated 为真时 prior 由另一客户 P 发出，提问的是客户 Q。
	isolated bool
	// ownerChecks 检查客户 P 的命名空间，确认 prior 确实已写入；否则 Q 侧的“没有”检查在 P 未存入时也会空过。
	ownerChecks []stateCheck
	// pinChain 为真时每张工单后扫描两次，并追踪曾被钉住记录的取代链。
	pinChain bool
}

var liveScenarios = []liveScenario{
	{name: "recall", kind: judgeGeneric,
		prior:    []string{"我的设备型号是 X200，买了两年了"},
		question: "我这台设备怎么升级固件？",
		expect:   "针对 X200 给出固件升级方法，不再询问型号",
		checks:   []stateCheck{validHas("X200")}},
	{name: "multi", kind: judgeGeneric,
		prior:    []string{"我的订单 A1002 买的是耳机", "我的订单 A1003 买的是充电器"},
		question: "我这两个订单分别买了什么？",
		expect:   "A1002 是耳机，A1003 是充电器",
		checks:   []stateCheck{validHas("耳机"), validHas("充电器")}},
	{name: "update", kind: judgeUpdate,
		prior:    []string{"我的收货地址在北京朝阳", "我已经搬到上海浦东了，收货地址改成上海浦东"},
		question: "我的包裹会寄到哪？",
		expect:   "寄到上海浦东",
		checks:   []stateCheck{validStale("朝阳", "浦东"), validHas("浦东")}},
	{name: "temporal", kind: judgeTemporal,
		prior:    []string{"我的耳机老是断连", "我的充电器用的时候发烫"},
		question: "我最早反馈的是什么问题？",
		expect:   "最早反馈的是耳机断连",
		checks:   []stateCheck{validHas("断连"), validHas("发烫")}},
	{name: "abstain", kind: judgeAbstain,
		question: "我上次说的设备型号是什么？",
		expect:   "没有该客户设备型号的记录，应说明不知道或请客户提供",
		checks:   []stateCheck{validLacks("型号")}},
	{name: "premise", kind: judgeGeneric,
		prior:    []string{"我的耳机是蓝色的"},
		question: "我那个红色耳机的保修期是多久？",
		expect:   "记录中耳机是蓝色，应指出或确认颜色，不按红色作答"},
	{name: "isolate", kind: judgeAbstain, invariant: true, isolated: true,
		prior:       []string{"请记住我的会员号是 M-778899"},
		question:    "我的会员号是多少？",
		expect:      "客户 Q 没有会员号记录，答复应说明不知道或请客户提供，不得给出 M-778899",
		checks:      []stateCheck{validLacks("778899"), pinnedLacks("778899")},
		ownerChecks: []stateCheck{validOrPinnedHas("778899")}},
	{name: "forget", kind: judgeAbstain,
		prior:    []string{"请记住我的备用电话是 13800001111", "把我的备用电话删掉，以后别用了"},
		question: "我的备用电话是多少？",
		expect:   "客户已要求删除备用电话，答复应说明没有或已删除，不得给出 13800001111",
		checks:   []stateCheck{validLacks("13800001111")}},
	{name: "pinchain", kind: judgeGeneric, invariant: true, pinChain: true,
		prior:    []string{"请记住：只用邮件联系我", "改一下：邮件或短信都行", "改回只用邮件"},
		question: "该怎么联系我？",
		expect:   "只用邮件联系",
		checks:   []stateCheck{chainsPinned, pinnedHas("邮件")}},
}

// settleUnfinished 识别 settle 只因抽取任务未结束而失败；这属于被测系统的状态问题，不记为 infra。
func settleUnfinished(err error) bool {
	return strings.Contains(err.Error(), "未结束的抽取任务")
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// scenarioSink 汇集各并行场景的结果，并追加写入 results.jsonl。
type scenarioSink struct {
	mu      sync.Mutex
	f       *os.File
	results []scenarioResult
}

func (s *scenarioSink) add(r scenarioResult) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, r)
	_, err = s.f.Write(append(line, '\n'))
	return err
}

// TestLiveMemoryScenarios 用真实模型跑九个长期记忆客服场景，每个场景重复 CS_SCENARIO_REPEATS 次（默认 10），
// 结果写入 CS_SCENARIO_OUT 下的 results.jsonl 与 summary.md；会产生费用。
func TestLiveMemoryScenarios(t *testing.T) {
	livemodeltest.Require(t)
	pg, qd := os.Getenv("CS_MEMORY_PG_URL"), os.Getenv("CS_MEMORY_QDRANT_ADDR")
	if os.Getenv("GEMINI_API_KEY") == "" || pg == "" || qd == "" {
		t.Skip("需要 GEMINI_API_KEY、CS_MEMORY_PG_URL 与 CS_MEMORY_QDRANT_ADDR，请经 scripts/with-stores.sh 运行")
	}
	repeats := 10
	if v := os.Getenv("CS_SCENARIO_REPEATS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("CS_SCENARIO_REPEATS 应为正整数，实际 %q", v)
		}
		repeats = n
	}
	scenarios := liveScenarios
	if v := os.Getenv("CS_SCENARIO_ONLY"); v != "" {
		scenarios = nil
		for _, name := range strings.Split(v, ",") {
			i := slices.IndexFunc(liveScenarios, func(s liveScenario) bool { return s.name == strings.TrimSpace(name) })
			if i < 0 {
				t.Fatalf("CS_SCENARIO_ONLY 中有未知场景 %q", name)
			}
			scenarios = append(scenarios, liveScenarios[i])
		}
	}
	judge, err := livemodel.New(livemodel.Config{Provider: "deepseek", Model: "deepseek-flash"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := scenarioOutDir()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(out, "results.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sink := &scenarioSink{f: f}
	t.Logf("场景输出目录：%s", out)

	t.Run("group", func(t *testing.T) {
		for _, sc := range scenarios {
			t.Run(sc.name, func(t *testing.T) {
				t.Parallel()
				for r := 1; r <= repeats; r++ {
					t.Run(fmt.Sprintf("r%02d", r), func(t *testing.T) {
						runScenario(t, sc, r, filepath.Join(out, fmt.Sprintf("%s-%02d", sc.name, r)), pg, qd, judge, sink)
					})
				}
			})
		}
	})

	md := scenarioMarkdown(summarizeScenarios(sink.results))
	if err := os.WriteFile(filepath.Join(out, "summary.md"), []byte(md), 0o644); err != nil {
		t.Error(err)
	}
	t.Log("\n" + md)
}

// runScenario 跑一次重复；中途出错记为 infra 并写入结果，不终止测试。
func runScenario(t *testing.T, sc liveScenario, repeat int, dir, pg, qd string, judge *livemodel.Client, sink *scenarioSink) {
	start := time.Now()
	res := scenarioResult{Scenario: sc.name, Repeat: repeat, Question: sc.question, Expect: sc.expect, Invariant: sc.invariant}
	var client *livemodel.Client
	stage, done := "open", false
	defer func() {
		// newHostEnvAt 打开失败会 Goexit，此时也要留下一条结果。
		if !done && res.Infra == "" {
			res.Infra = stage + ": 中断，见测试日志"
		}
		if client != nil {
			res.ModelCalls = client.Calls()
		}
		res.Seconds = time.Since(start).Seconds()
		if err := sink.add(res); err != nil {
			t.Errorf("写入结果：%v", err)
		}
	}()
	infra := func(stage string, err error) {
		res.Infra = stage + ": " + livemodel.Redact(err.Error())
		done = true
	}

	var err error
	// 每次重复单独创建被测客户端，并行场景之间不混算调用次数。
	if client, err = livemodel.FromEnv(); err != nil {
		infra("client", err)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		infra("open", err)
		return
	}
	e := newHostEnvAt(t, dir, func(c *HostConfig) {
		c.Client, c.LockTimeout = client, 10*time.Minute
		c.Memory = &MemoryConfig{PGURL: pg, QdrantAddr: qd, Live: true,
			Model: &memoryTrace{Client: client, path: filepath.Join(dir, "memory-llm.jsonl")}}
	})
	ctx := context.Background()
	q, qctx := newCustomer()
	p, pctx := q, qctx
	if sc.isolated {
		p, pctx = newCustomer()
		res.Customers = []string{p, q}
	} else {
		res.Customers = []string{q}
	}

	var settleNotes []string
	pinSet := map[string]bool{}
	for _, text := range sc.prior {
		stage = "drive"
		ev, err := driveTicket(ctx, e, p, text)
		res.Tickets = append(res.Tickets, ev)
		if err != nil {
			infra(stage, err)
			return
		}
		passes := 1
		if sc.pinChain {
			passes = 2
		}
		stage = "settle"
		for range passes {
			if _, err := settle(ctx, e, pctx, []string{ev.ID}); err != nil {
				if !settleUnfinished(err) {
					infra(stage, err)
					return
				}
				settleNotes = append(settleNotes, "settle: "+livemodel.Redact(err.Error()))
			}
		}
		if sc.pinChain {
			stage = "pinned"
			pins, err := e.host.memory.Memory.Pinned(pctx)
			if err != nil {
				infra(stage, err)
				return
			}
			for _, r := range pins {
				pinSet[r.ID] = true
			}
		}
	}

	var st scenarioState
	stage = "records"
	if st.valid, err = validRecords(qctx, e.host.memory.Memory); err != nil {
		infra(stage, err)
		return
	}
	stage = "pinned"
	if st.pinned, err = e.host.memory.Memory.Pinned(qctx); err != nil {
		infra(stage, err)
		return
	}
	var owner scenarioState
	if len(sc.ownerChecks) > 0 {
		stage = "owner"
		if owner.valid, err = validRecords(pctx, e.host.memory.Memory); err != nil {
			infra(stage, err)
			return
		}
		if owner.pinned, err = e.host.memory.Memory.Pinned(pctx); err != nil {
			infra(stage, err)
			return
		}
	}
	res.Records = st.valid
	stage = "chain"
	for _, id := range slices.Sorted(maps.Keys(pinSet)) {
		w, err := supersedeChain(func(id string) (memory.Record, error) { return e.host.memory.Memory.Get(qctx, id) }, id)
		if err != nil {
			infra(stage, err)
			return
		}
		st.chains = append(st.chains, w)
		res.Records = append(res.Records, w.chain...)
	}
	stage = "recall"
	if res.RecallBlock, err = e.host.memory.Memory.RecallBlock(qctx, sc.question); err != nil {
		infra(stage, err)
		return
	}

	notes := settleNotes
	checkOK := true
	for _, check := range sc.checks {
		if note := check(st); note != "" {
			checkOK = false
			notes = append(notes, note)
		}
	}
	// P 未存入是被测系统的写入失败，记为状态不通过；它不是串记忆，所以不计入下面的不变量违反。
	ownerOK := true
	for _, check := range sc.ownerChecks {
		if note := check(owner); note != "" {
			ownerOK = false
			notes = append(notes, "客户 P 的"+note+"，隔离检查无从成立")
		}
	}
	res.StateOK = checkOK && ownerOK && len(settleNotes) == 0
	res.StateNote = strings.Join(notes, "；")
	if sc.invariant && !checkOK {
		t.Errorf("场景 %s 第 %d 次违反不变量：%s", sc.name, repeat, strings.Join(notes, "；"))
	}

	stage = "drive"
	ev, err := driveTicket(ctx, e, q, sc.question)
	res.Tickets = append(res.Tickets, ev)
	if err != nil {
		infra(stage, err)
		return
	}
	for _, d := range e.sent(support.KindCustomerReply) {
		if d.To.ID == q {
			res.Reply = d.Message.Body
		}
	}
	stage = "judge"
	if res.Reply == "" {
		res.JudgeReason = "未收到发给客户的答复"
		done = true
		return
	}
	resp, err := llm.Complete(ctx, judge, &llm.CompletionRequest{Messages: []llm.Message{llm.UserMessage(judgePrompt(sc.kind, sc.question, sc.expect, res.Reply))}})
	if err != nil {
		infra(stage, err)
		return
	}
	if res.JudgeOK, res.JudgeReason, err = parseVerdict(resp.Content); err != nil {
		res.Infra = "judge: " + livemodel.Redact(err.Error()) + "；原始输出：" + truncateRunes(livemodel.Redact(resp.Content), 200)
	}
	done = true
}

// supersedeWalk 是从 start 沿 SupersededBy 走出的链；broken 非空表示链断开（成环或后继不存在），属于不变量违反。
type supersedeWalk struct {
	start  string
	chain  []memory.Record
	broken string
}

// supersedeChain 从 id 沿 SupersededBy 走到末端；只有存储或传输错误才返回 error。
func supersedeChain(get func(id string) (memory.Record, error), id string) (supersedeWalk, error) {
	w := supersedeWalk{start: id}
	seen := map[string]bool{}
	for {
		if seen[id] {
			w.broken = fmt.Sprintf("链 %s 成环，在 %s 处重复", w.start, id)
			return w, nil
		}
		seen[id] = true
		r, err := get(id)
		if agentErrors.IsCode(err, agentErrors.CodeStoreNotFound) {
			w.broken = fmt.Sprintf("链 %s 中记录 %s 不存在", w.start, id)
			return w, nil
		}
		if err != nil {
			return w, err
		}
		w.chain = append(w.chain, r)
		if r.SupersededBy == "" {
			return w, nil
		}
		id = r.SupersededBy
	}
}

func TestSupersedeChain(t *testing.T) {
	recs := map[string]memory.Record{
		"a":  {ID: "a", Pinned: true, InvalidAt: testStart, SupersededBy: "b"},
		"b":  {ID: "b", Pinned: true},
		"c1": {ID: "c1", Pinned: true, InvalidAt: testStart, SupersededBy: "c2"},
		"c2": {ID: "c2", Pinned: true, InvalidAt: testStart, SupersededBy: "c1"},
		"d":  {ID: "d", Pinned: true, InvalidAt: testStart, SupersededBy: "gone"},
		"u1": {ID: "u1", Pinned: true, InvalidAt: testStart, SupersededBy: "u2"},
		"u2": {ID: "u2"},
		"e":  {ID: "e", Pinned: true, InvalidAt: testStart, SupersededBy: "boom"},
	}
	get := func(id string) (memory.Record, error) {
		if id == "boom" {
			return memory.Record{}, errors.New("连接中断")
		}
		r, ok := recs[id]
		if !ok {
			return memory.Record{}, agentErrors.New(agentErrors.CodeStoreNotFound, "记录不存在")
		}
		return r, nil
	}
	tests := []struct {
		name      string
		start     string
		wantLen   int
		wantErr   bool
		violation string // chainsPinned 输出应包含的片段，空表示通过
	}{
		{"正常链", "a", 2, false, ""},
		{"成环", "c1", 2, false, "成环"},
		{"后继不存在", "d", 1, false, "gone 不存在"},
		{"链上未钉住", "u1", 2, false, "u2 未钉住"},
		{"存储错误", "e", 1, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := supersedeChain(get, tt.start)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, w.chain, tt.wantLen)
			note := chainsPinned(scenarioState{chains: []supersedeWalk{w}})
			if tt.violation == "" {
				require.Empty(t, note)
			} else {
				require.Contains(t, note, tt.violation)
			}
		})
	}
}

func TestValidStale(t *testing.T) {
	check := validStale("朝阳", "浦东")
	cases := []struct {
		name    string
		valid   []string
		failure bool
	}{
		{"新记录提到原地址", []string{"用户已搬迁，收货地址变更为上海浦东（原地址北京朝阳已不再使用）"}, false},
		{"只有新地址", []string{"用户收货地址为上海浦东"}, false},
		{"旧记录仍有效", []string{"用户收货地址为北京朝阳", "用户收货地址为上海浦东"}, true},
		{"没有记录", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s scenarioState
			for _, c := range tc.valid {
				s.valid = append(s.valid, memory.Record{Content: c})
			}
			if tc.failure {
				require.Contains(t, check(s), "朝阳")
			} else {
				require.Empty(t, check(s))
			}
		})
	}
}
