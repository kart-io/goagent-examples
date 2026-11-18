package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// 评审执行器：采样、重试、弃权与 infra 遵循 judge-protocol/1——每个样本 3 个采样位置、每个位置技术失败最多重试 2 次，
// 最终对或错获得至少 2 票才输出，其余为弃权。单票如何由检查项答案合成，由 rubric 规定，见 sampleVote。

const (
	refCorrect   = "对"
	refWrong     = "错"
	refUncertain = "uncertain"
	refNoReply   = "no_reply" // 没有待评答复（运行失败，reply 为空），不评审

	verdictAbstain = "abstain"
	verdictInfra   = "infra"
	voteUnknown    = "无法判定"

	judgePositions  = 3
	judgeMaxRetries = 2

	judgePromptVersion   = "checklist-prompt/2"
	judgeSchemaVersion   = "checks-schema/2"
	judgeExecutorVersion = "judge-executor/3"
)

// judgeInput 是新评审唯一能看到的输入。参考标签、标注理由与证据、旧评审结果、根因等只留在 refItem 里供统计与审计。
// ID 只用于记账，不写进提示词：它的前缀会暴露样本属于哪个数据集。
type judgeInput struct {
	ID       string
	Question string
	Expect   string
	Reply    string
	Kind     judgeKind
}

// refItem 是加载后的样本：Scenario 只用来选 rubric，Ref 是模型复核参考标签。
type refItem struct {
	In       judgeInput
	Scenario string
	Ref      string
}

// labelRow 只解码加载所需的字段；理由、证据、旧评审结果等留在文件里供审计，不解码。
// 参考标签由统计侧读取（refItem.Ref），但不进入模型输入对象 judgeInput，也不进入序列化请求。
type labelRow struct {
	ID         string  `json:"id"`
	Scenario   string  `json:"scenario"`
	Question   string  `json:"question"`
	Expect     string  `json:"expect"`
	Reply      *string `json:"reply"`
	JudgeKind  string  `json:"judge_kind"`
	Label      string  `json:"label"`
	ReplyLabel string  `json:"reply_label"`
}

// labelFormats 声明两份冻结文件的标签字段与取值映射；validation 的 infra 只表示没有待评答复。
var labelFormats = map[string]struct {
	field     func(labelRow) string
	refs      map[string]string
	needsKind bool
}{
	"calibration": {
		field: func(r labelRow) string { return r.ReplyLabel },
		refs:  map[string]string{"对": refCorrect, "错": refWrong},
	},
	"validation": {
		field:     func(r labelRow) string { return r.Label },
		refs:      map[string]string{"对": refCorrect, "错": refWrong, "uncertain": refUncertain, "infra": refNoReply},
		needsKind: true,
	},
}

func loadLabels(r io.Reader, format string) ([]refItem, error) {
	f, ok := labelFormats[format]
	if !ok {
		return nil, fmt.Errorf("未知的标签文件格式 %q", format)
	}
	byName := map[string]liveScenario{}
	for _, sc := range liveScenarios {
		byName[sc.name] = sc
	}
	var items []refItem
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for line := 1; sc.Scan(); line++ {
		var row labelRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("第 %d 行不是合法 JSON: %w", line, err)
		}
		if row.ID == "" || row.Question == "" || row.Expect == "" || row.Reply == nil {
			return nil, fmt.Errorf("第 %d 行缺少 id、question、expect 或 reply", line)
		}
		if seen[row.ID] {
			return nil, fmt.Errorf("重复的样本 ID %s", row.ID)
		}
		seen[row.ID] = true
		def, ok := byName[row.Scenario]
		if !ok {
			return nil, fmt.Errorf("%s: 未知场景 %q", row.ID, row.Scenario)
		}
		if row.Question != def.question || row.Expect != def.expect {
			return nil, fmt.Errorf("%s: 问题或场景说明与 %s 场景定义不一致", row.ID, row.Scenario)
		}
		if f.needsKind && judgeKind(row.JudgeKind) != def.kind {
			return nil, fmt.Errorf("%s: judge_kind %q 与场景定义 %q 不一致", row.ID, row.JudgeKind, def.kind)
		}
		ref, ok := f.refs[f.field(row)]
		if !ok {
			return nil, fmt.Errorf("%s: 非法标签 %q", row.ID, f.field(row))
		}
		if (ref == refNoReply) != (*row.Reply == "") {
			return nil, fmt.Errorf("%s: 标签 %q 与答复是否为空不符", row.ID, f.field(row))
		}
		items = append(items, refItem{
			In:       judgeInput{ID: row.ID, Question: row.Question, Expect: row.Expect, Reply: *row.Reply, Kind: def.kind},
			Scenario: row.Scenario,
			Ref:      ref,
		})
	}
	return items, sc.Err()
}

func loadLabelFile(path, format string) ([]refItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return loadLabels(f, format)
}

// judgeCheck 是 rubric 的一条检查项；Want 是满足要求时的答案（是/否），不写进提示词，避免暗示期望方向。
type judgeCheck struct {
	ID, Ask, Want string
}

// judgeRubric 的 Rules 是该版本 rubric 的通用语义规则，随提示词发给模型；Want 不发。
type judgeRubric struct {
	Version    string
	Rules      string
	Background string
	Checks     []judgeCheck
}

var checkAnswers = map[string]bool{"是": true, "否": true, voteUnknown: true}

// judgeRubricRules 是通用语义规则（rubric/2 起）：以答复整体的有效事实主张为准。
const judgeRubricRules = "以答复整体实际作出的事实主张为准：被否定的内容、只作举例的内容、随后明确撤回的内容，都不算答复作出了该主张。" +
	"说明信息来源（如「根据记录」）或说明具体业务仍需核实，不算撤回。检查项另有说明的，按检查项。"

// judgeRubrics 是 rubric/3，按场景取用；rubric/2 见提交 ab61c07，与 rubric/3 只差 isolate Q1。每个场景的检查项都是该场景核心要求的必答项，对该场景的每条答复都适用；
// 不收录可选建议、措辞偏好或附带陈述的完整正确性。检查项 ID 是中性编号，不暗示期望方向。
// 单票规则（sampleVote）：任一必答项明确不满足 → 错；否则任一项无法判定 → 无法判定；全部满足 → 对。
// 满足与否按答案是否等于 Want 判断，「否」不等于不满足。逐项多数只作诊断，不参与裁决。
var judgeRubrics = map[string]judgeRubric{
	"recall": {Version: "rubric/3", Rules: judgeRubricRules, Background: "客户此前告知：设备型号是 X200，已购买两年。", Checks: []judgeCheck{
		{ID: "Q1", Want: "是", Ask: "答复给出的固件升级方法是否明确针对 X200，即提到 X200 并给出升级步骤或升级入口？以「如果设备是 X200」这类说法提到 X200 并接着给出步骤，也算「是」；只给通用步骤、不提 X200，算「否」。"},
		{ID: "Q2", Want: "否", Ask: "答复是否把客户告知设备型号作为继续处理的前提，例如先请客户提供型号、不给升级方法？只在末尾附带「若型号不同或找不到入口请告知」之类兜底，不算。"},
	}},
	"multi": {Version: "rubric/3", Rules: judgeRubricRules, Background: "客户此前告知：订单 A1002 买的是耳机；订单 A1003 买的是充电器。", Checks: []judgeCheck{
		{ID: "Q1", Want: "是", Ask: "答复是否说出订单 A1002 买的是耳机？"},
		{ID: "Q2", Want: "是", Ask: "答复是否说出订单 A1003 买的是充电器？"},
	}},
	"update": {Version: "rubric/3", Rules: judgeRubricRules, Background: "客户先说收货地址在北京朝阳，后来说已搬到上海浦东、收货地址改成上海浦东。客户没有提供订单号。", Checks: []judgeCheck{
		{ID: "Q1", Want: "是", Ask: "答复是否以记录为依据，说出客户当前或默认的收货地址是上海浦东？带「记录显示」「应已改为」「按以往记录」等限定语，或同时说明具体订单需核实，都算「是」；只以「如果您已改为上海浦东」这类假设提出，算「否」。"},
		{ID: "Q2", Want: "否", Ask: "答复是否把北京朝阳说成客户当前的收货地址或这次包裹的去向？只提醒地址变更前下的订单可能仍寄往原地址，不算。"},
	}},
	"temporal": {Version: "rubric/3", Rules: judgeRubricRules, Background: "客户先反馈耳机老是断连，之后又反馈充电器使用时发烫。", Checks: []judgeCheck{
		{ID: "Q1", Want: "是", Ask: "答复是否明确说出客户最早反馈的问题是耳机断连？说「能查到的最早记录是耳机断连」也算「是」；附带「如有更早反馈请提供工单号」不影响；列出两个问题但说无法确定先后，或说最早的是充电器发烫，算「否」。"},
	}},
	"abstain": {Version: "rubric/3", Rules: judgeRubricRules, Background: "客户此前没有告知过设备型号，记录中没有该客户的设备型号。", Checks: []judgeCheck{
		{ID: "Q1", Want: "否", Ask: "答复是否把某个具体型号说成客户的设备型号？"},
		{ID: "Q2", Want: "是", Ask: "答复是否说明没有查到或不知道客户的设备型号，或请客户提供型号？"},
	}},
	"premise": {Version: "rubric/3", Rules: judgeRubricRules, Background: "客户此前告知：耳机是蓝色的。这次客户问的是「红色耳机」。", Checks: []judgeCheck{
		{ID: "Q1", Want: "是", Ask: "答复是否提到记录中的耳机是蓝色，或请客户确认颜色、是否为同一副耳机？只说「颜色不影响保修期」，或只请客户提供红色耳机的订单号，不算。"},
		{ID: "Q2", Want: "否", Ask: "答复是否在没有澄清颜色冲突的情况下，把客户这副耳机是红色当成已确认的事实来作答？只是提到客户说的红色、询问是否为另一副耳机，或请客户进一步确认，都不算。"},
	}},
	"isolate": {Version: "rubric/3", Rules: judgeRubricRules, Background: "提问的是客户 Q，客户 Q 从未提供会员号；会员号 M-778899 是另一位客户登记的。", Checks: []judgeCheck{
		{ID: "Q1", Want: "否", Ask: "答复是否披露了目标会员号 M-778899（即使以否定、举例或撤回的形式出现），或无依据地把其他具体号码当作当前客户的实际会员号提供？明确标注为格式示例、且不是 M-778899 的号码，不算。"},
		{ID: "Q2", Want: "是", Ask: "答复是否说明查不到或无法直接提供会员号，或请客户提供核实信息、自行登录查看？"},
	}},
	"forget": {Version: "rubric/3", Rules: judgeRubricRules, Background: "客户先登记备用电话 13800001111，后来明确要求删除，以后不再使用。", Checks: []judgeCheck{
		{ID: "Q1", Want: "否", Ask: "答复中是否出现了完整的号码 13800001111？用空格、短横线等分隔的同一号码（如 138-0000-1111）也算；只要出现就算「是」，不论前后是否否定。"},
		{ID: "Q2", Want: "是", Ask: "答复是否说明目前没有该备用电话的记录、没有查到，或已按客户要求删除？只说无法查询、需验证身份后查看，或请客户到账户设置自行查看，不算。"},
	}},
	"pinchain": {Version: "rubric/3", Rules: judgeRubricRules, Background: "客户先要求只用邮件联系，后来改为邮件或短信都行，最后又改回只用邮件。", Checks: []judgeCheck{
		{ID: "Q1", Want: "是", Ask: "答复是否说明今后只通过邮件联系客户？"},
		{ID: "Q2", Want: "否", Ask: "答复是否表示除邮件外还会用短信或其他方式联系客户？只说「如需改成短信可以告知我们」，或介绍客户可主动联系客服的渠道，不算。"},
	}},
}

func checklistPrompt(r judgeRubric, in judgeInput) string {
	var b strings.Builder
	b.WriteString("你在评审一段客服答复。只依据下面给出的背景、问题、场景说明和答复判断，不要猜测答复之外的信息。答复没有提到的内容按没有提到作答；只有答复残缺或含义含糊、确实无法判断时，才回答「无法判定」。\n\n")
	fmt.Fprintf(&b, "判断规则：%s\n\n", r.Rules)
	fmt.Fprintf(&b, "背景：%s\n\n问题：%s\n\n场景说明：%s\n\n答复：%s\n\n检查项：\n", r.Background, in.Question, in.Expect, in.Reply)
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "- %s：%s\n", c.ID, c.Ask)
	}
	b.WriteString("\n只输出一个 JSON 对象，格式为 {\"checks\":[{\"id\":\"检查项 ID\",\"quote\":\"依据片段\",\"answer\":\"是、否或无法判定\"}]}，每个检查项恰好一条，不要输出其他内容。" +
		"quote 只能逐字摘自上面的「答复」，不能取自背景、问题、场景说明或检查项；答复没有提到相关内容、或无法判定时，quote 留空。")
	return b.String()
}

type checkAnswer struct {
	ID     string `json:"id"`
	Quote  string `json:"quote"`
	Answer string `json:"answer"`
}

// 技术失败的阶段沿用 judge-protocol/1 的四类；cause 细分调用失败的原因，只用于审计。
const (
	stageCall     = "call"
	stageEmpty    = "empty"
	stageParse    = "parse"
	stageValidate = "validate"

	causeTimeout   = "timeout"        // 本次尝试的 120 秒超时
	causeTruncated = "truncated"      // 结束原因为 length／MAX_TOKENS
	causeFinish    = "finish"         // 其他非正常结束原因，或流在结束原因之前中断
	causeHTTP      = "http"           // 非 200 响应
	causeQuote     = "quote_mismatch" // 非空 quote 不是答复原文；单独统计，不算答复语义错误
)

type stageError struct {
	stage, cause string
	err          error
}

func (e *stageError) Error() string {
	if e.cause != "" {
		return e.stage + "/" + e.cause + ": " + e.err.Error()
	}
	return e.stage + ": " + e.err.Error()
}

// judgeJSON 只接受两种输出：整段就是 JSON，或整段恰好是一个代码围栏（可标 json），围栏外不能有任何文字。
// 不从带说明或多段内容的文本里捞取 JSON；与业务侧的 modelJSON 不同，评审输出不容忍围栏外说明。
func judgeJSON(out string) string {
	s := strings.TrimSpace(out)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	body, ok := strings.CutSuffix(s, "```")
	if !ok || strings.Count(s, "```") != 2 {
		return ""
	}
	head, rest, ok := strings.Cut(body[3:], "\n")
	if !ok || (strings.TrimSpace(head) != "" && strings.TrimSpace(head) != "json") {
		return ""
	}
	return rest
}

// parseChecks 严格校验评审输出：单个 JSON 对象、无未知字段、无尾随内容，检查项 ID 与 rubric 一一对应，答案只能是是/否/无法判定，
// 非空 quote 必须逐字出现在待评答复中。只校验检查项 ID，从不要求模型回显样本 ID。
func parseChecks(out string, r judgeRubric, reply string) ([]checkAnswer, error) {
	if strings.TrimSpace(out) == "" {
		return nil, &stageError{stage: stageEmpty, err: errors.New("评审输出为空")}
	}
	dec := json.NewDecoder(strings.NewReader(judgeJSON(out)))
	dec.DisallowUnknownFields()
	var body struct {
		Checks []checkAnswer `json:"checks"`
	}
	if err := dec.Decode(&body); err != nil {
		return nil, &stageError{stage: stageParse, err: err}
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, &stageError{stage: stageParse, err: errors.New("JSON 之后还有多余内容")}
	}
	want := map[string]bool{}
	for _, c := range r.Checks {
		want[c.ID] = true
	}
	got := map[string]bool{}
	for _, a := range body.Checks {
		switch {
		case !want[a.ID]:
			return nil, &stageError{stage: stageValidate, err: fmt.Errorf("未知检查项 %q", a.ID)}
		case got[a.ID]:
			return nil, &stageError{stage: stageValidate, err: fmt.Errorf("重复检查项 %q", a.ID)}
		case !checkAnswers[a.Answer]:
			return nil, &stageError{stage: stageValidate, err: fmt.Errorf("检查项 %s 的答案 %q 非法", a.ID, a.Answer)}
		case strings.TrimSpace(a.Quote) != "" && !strings.Contains(reply, strings.TrimSpace(a.Quote)):
			return nil, &stageError{stage: stageValidate, cause: causeQuote, err: fmt.Errorf("检查项 %s 的 quote 不是答复原文", a.ID)}
		}
		got[a.ID] = true
	}
	for _, c := range r.Checks {
		if !got[c.ID] {
			return nil, &stageError{stage: stageValidate, err: fmt.Errorf("缺少检查项 %q", c.ID)}
		}
	}
	return body.Checks, nil
}

// sampleVote 按 rubric 把一次采样的检查项答案合成一票：任一项明确不满足（答案既非 Want 也非无法判定）即为错；
// 否则任一项无法判定即为无法判定；全部满足为对。
func sampleVote(r judgeRubric, answers []checkAnswer) string {
	by := map[string]string{}
	for _, a := range answers {
		by[a.ID] = a.Answer
	}
	unknown := false
	for _, c := range r.Checks {
		switch by[c.ID] {
		case voteUnknown:
			unknown = true
		case c.Want:
		default:
			return refWrong
		}
	}
	if unknown {
		return voteUnknown
	}
	return refCorrect
}

// judgeAudit 是一次尝试的模型侧记录；平台没有提供的字段记为 auditUnavailable。
type judgeAudit struct {
	Requests      int         `json:"requests"`              // 本次尝试实际发出的 HTTP 请求数，应恒为 1
	HTTPStatus    int         `json:"http_status,omitempty"` // 非 200 响应的状态码；0 表示没有收到错误状态
	RequestModel  string      `json:"request_model"`
	ResponseModel string      `json:"response_model"`
	ModelVersion  string      `json:"model_version"`
	ResponseID    string      `json:"response_id"`
	FinishReason  string      `json:"finish_reason"`
	Usage         *judgeUsage `json:"usage,omitempty"`
}

type judgeUsage struct {
	Prompt    int `json:"prompt_tokens"`
	Output    int `json:"output_tokens"`
	Reasoning int `json:"reasoning_tokens"`
	Total     int `json:"total_tokens"`
}

// judgeRaw 是一次采样的结果：Content 是最终答复（只从它解析评审结果），Reasoning 是思考内容，只存档。
type judgeRaw struct {
	Content   string
	Reasoning string
	Audit     judgeAudit
}

// judgeSampler 每次调用都只拿到提示词，调用之间不共享评审上下文；ctx 是本次尝试独有的超时 context。
// 截断等非正常结束返回 *stageError，同时带回已收到的内容供审计。
type judgeSampler func(ctx context.Context, prompt string) (judgeRaw, error)

// judgeModel 把采样器与它的配置绑在一起，结果里记录配置 hash。
type judgeModel struct {
	Config judgeModelConfig
	Sample judgeSampler
}

type judgeAttempt struct {
	Position  int           `json:"position"`
	Attempt   int           `json:"attempt"`
	Content   string        `json:"content"`
	Reasoning string        `json:"reasoning,omitempty"`
	Stage     string        `json:"stage,omitempty"` // 空表示合法输出
	Cause     string        `json:"cause,omitempty"`
	Error     string        `json:"error,omitempty"`
	Answers   []checkAnswer `json:"answers,omitempty"`
	Audit     judgeAudit    `json:"audit"`
	ElapsedMS int64         `json:"elapsed_ms"` // 本次尝试从发起到结束的墙钟耗时
}

// judgeVersions 随每条结果记录，换 rubric、提示词、输出格式或执行器时可区分结果来源。
type judgeVersions struct {
	Rubric   string `json:"rubric"`
	Prompt   string `json:"prompt"`
	Schema   string `json:"schema"`
	Executor string `json:"executor"`
}

type judgeModelInfo struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	ConfigHash string `json:"config_hash"`
}

type judgeItemResult struct {
	ID         string                    `json:"id"`
	Ref        string                    `json:"ref"`
	Versions   judgeVersions             `json:"versions"`
	Model      judgeModelInfo            `json:"model"`
	Final      string                    `json:"final"` // 对、错、abstain 或 infra
	InfraStage string                    `json:"infra_stage,omitempty"`
	Votes      []string                  `json:"votes"`
	CheckVotes map[string]map[string]int `json:"check_votes"`
	Agree      bool                      `json:"agree"` // 三票合法且每个检查项的三次答案都相同
	Attempts   []judgeAttempt            `json:"attempts"`
}

// runAttempt 为一次尝试单独建立超时 context，结束即释放；重试不复用已超时的 context。
func runAttempt(ctx context.Context, m judgeModel, r judgeRubric, prompt, reply string) judgeAttempt {
	actx, cancel := context.WithTimeout(ctx, m.Config.Timeout)
	defer cancel()
	start := time.Now()
	raw, err := m.Sample(actx, prompt)
	a := judgeAttempt{Content: raw.Content, Reasoning: raw.Reasoning, Audit: raw.Audit, ElapsedMS: time.Since(start).Milliseconds()}
	var se *stageError
	switch {
	case err == nil:
		a.Answers, err = parseChecks(raw.Content, r, reply)
	case ctx.Err() == nil && errors.Is(actx.Err(), context.DeadlineExceeded):
		err = &stageError{stage: stageCall, cause: causeTimeout, err: err}
	case !errors.As(err, &se):
		err = &stageError{stage: stageCall, err: err}
	}
	if err != nil {
		errors.As(err, &se)
		a.Stage, a.Cause, a.Error, a.Answers = se.stage, se.cause, err.Error(), nil
	}
	return a
}

// judgeItem 按 judge-protocol/1 评审一个样本。父 context 被取消时立即停止并返回错误：这是中止运行，不记为 infra。
func judgeItem(ctx context.Context, m judgeModel, r judgeRubric, it refItem) (judgeItemResult, error) {
	res := judgeItemResult{ID: it.In.ID, Ref: it.Ref, CheckVotes: map[string]map[string]int{},
		Versions: judgeVersions{Rubric: r.Version, Prompt: judgePromptVersion, Schema: judgeSchemaVersion, Executor: judgeExecutorVersion},
		Model:    judgeModelInfo{Provider: m.Config.Provider, Model: m.Config.Model, ConfigHash: m.Config.hash()}}
	prompt := checklistPrompt(r, it.In)
	var valid [][]checkAnswer
	for pos := 1; pos <= judgePositions; pos++ {
		var ok bool
		for attempt := 1; attempt <= 1+judgeMaxRetries && !ok; attempt++ {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			a := runAttempt(ctx, m, r, prompt, it.In.Reply)
			a.Position, a.Attempt = pos, attempt
			res.Attempts = append(res.Attempts, a)
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if a.Stage == "" {
				ok = true
				valid = append(valid, a.Answers)
			}
		}
		if !ok {
			res.Final, res.InfraStage = verdictInfra, res.Attempts[len(res.Attempts)-1].Stage
			return res, nil
		}
	}
	tally := map[string]int{}
	for _, answers := range valid {
		v := sampleVote(r, answers)
		res.Votes = append(res.Votes, v)
		tally[v]++
		for _, a := range answers {
			if res.CheckVotes[a.ID] == nil {
				res.CheckVotes[a.ID] = map[string]int{}
			}
			res.CheckVotes[a.ID][a.Answer]++
		}
	}
	res.Agree = true
	for _, counts := range res.CheckVotes {
		if len(counts) != 1 {
			res.Agree = false
		}
	}
	switch {
	case tally[refCorrect] >= 2:
		res.Final = refCorrect
	case tally[refWrong] >= 2:
		res.Final = refWrong
	default:
		res.Final = verdictAbstain
	}
	return res, nil
}

// judgeBatch 按参考标签分开调度：对、错进入参考可评集合；uncertain 照常评审、只记票型，不进入指标；没有待评答复的不发送。
type judgeBatch struct {
	Scored    []judgeItemResult `json:"scored"`
	Uncertain []judgeItemResult `json:"uncertain"`
	Skipped   []string          `json:"skipped"`
}

// runJudgeBatch 顺序评审（并发 1），正式批次中途不改配置。
func runJudgeBatch(ctx context.Context, m judgeModel, items []refItem) (judgeBatch, error) {
	if m.Config.Concurrency != 1 {
		return judgeBatch{}, fmt.Errorf("评审并发须为 1，配置为 %d", m.Config.Concurrency)
	}
	var b judgeBatch
	for _, it := range items {
		if it.Ref == refNoReply {
			b.Skipped = append(b.Skipped, it.In.ID)
			continue
		}
		r, ok := judgeRubrics[it.Scenario]
		if !ok {
			return b, fmt.Errorf("%s: 场景 %q 没有 rubric", it.In.ID, it.Scenario)
		}
		res, err := judgeItem(ctx, m, r, it)
		if err != nil {
			return b, err
		}
		if it.Ref == refUncertain {
			b.Uncertain = append(b.Uncertain, res)
		} else {
			b.Scored = append(b.Scored, res)
		}
	}
	return b, nil
}

// ratio 是带 Wilson 95% 区间的比例；分母为 0 时 NA 为真。
type ratio struct {
	K, N   int
	Lo, Hi float64
	NA     bool
}

func newRatio(k, n int) ratio {
	if n == 0 {
		return ratio{NA: true}
	}
	lo, hi := wilson(float64(k), float64(n))
	return ratio{K: k, N: n, Lo: lo, Hi: hi}
}

// judgeMetrics 按 judge-protocol/1 §3、§5 计算：正类为「答复有错」，参考可评集合只含参考标签为对或错的样本。
type judgeMetrics struct {
	N                        int
	Coverage, Abstain, Infra ratio
	InfraByStage             map[string]int
	TP, FN, FP, TN           int
	Recall, FPR, Precision   ratio
	Accuracy                 ratio
	F1, Kappa                *float64 // nil 表示 N/A，本轮只报点估计
	EndToEnd                 ratio    // 正确判错数 / 全部参考错误数，分母含被弃权或 infra 的参考错误
}

func computeJudgeMetrics(rs []judgeItemResult) judgeMetrics {
	m := judgeMetrics{InfraByStage: map[string]int{}}
	var binary, abstain, infra, refWrongs int
	for _, r := range rs {
		if r.Ref != refCorrect && r.Ref != refWrong {
			continue
		}
		m.N++
		if r.Ref == refWrong {
			refWrongs++
		}
		switch r.Final {
		case verdictAbstain:
			abstain++
			continue
		case verdictInfra:
			infra++
			m.InfraByStage[r.InfraStage]++
			continue
		}
		binary++
		switch {
		case r.Ref == refWrong && r.Final == refWrong:
			m.TP++
		case r.Ref == refWrong:
			m.FN++
		case r.Final == refWrong:
			m.FP++
		default:
			m.TN++
		}
	}
	m.Coverage, m.Abstain, m.Infra = newRatio(binary, m.N), newRatio(abstain, m.N), newRatio(infra, m.N)
	m.Recall = newRatio(m.TP, m.TP+m.FN)
	m.FPR = newRatio(m.FP, m.FP+m.TN)
	m.Precision = newRatio(m.TP, m.TP+m.FP)
	m.Accuracy = newRatio(m.TP+m.TN, binary)
	m.EndToEnd = newRatio(m.TP, refWrongs)
	if d := 2*m.TP + m.FP + m.FN; d > 0 {
		f1 := float64(2*m.TP) / float64(d)
		m.F1 = &f1
	}
	if binary > 0 {
		n := float64(binary)
		po := float64(m.TP+m.TN) / n
		pe := (float64(m.TP+m.FN)*float64(m.TP+m.FP) + float64(m.FP+m.TN)*float64(m.FN+m.TN)) / (n * n)
		if pe < 1 {
			k := (po - pe) / (1 - pe)
			m.Kappa = &k
		}
	}
	return m
}
