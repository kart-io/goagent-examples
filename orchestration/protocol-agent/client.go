package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	api "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/uuid"
	a2aremote "github.com/kart-io/goagent/a2a/remote"
	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/interfaces"
	mg "github.com/kart-io/goagent/management"
	"github.com/kart-io/goagent/management/protocol"
	mcpremote "github.com/kart-io/goagent/mcp/remote"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 只向配置的同源服务发送令牌，避免发现卡或重定向把令牌带到其他主机。
type bearerTransport struct {
	origin, token string
	base          http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme+"://"+r.URL.Host != t.origin {
		return nil, fmt.Errorf("拒绝向发现地址之外的服务发送演示令牌")
	}
	copy := r.Clone(r.Context())
	copy.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(copy)
}
func (t bearerTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
func authenticatedClient(endpoint, token string) (*http.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || token == "" {
		return nil, fmt.Errorf("需要有效的服务根 URL 和 DEMO_TOKEN")
	}
	return &http.Client{Transport: bearerTransport{u.Scheme + "://" + u.Host, token, http.DefaultTransport.(*http.Transport).Clone()}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

type answers struct {
	auto    bool
	scanner *bufio.Scanner
	out     io.Writer
}

func (a *answers) text(ctx context.Context, prompt string) (string, error) {
	if _, err := fmt.Fprintln(a.out, prompt); err != nil {
		return "", err
	}
	if a.auto {
		return "离线合成发布说明", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// CLI 输入可阻塞；超时/信号先返回，run 负责关闭其拥有的可关闭输入源。
	type line struct {
		value string
		err   error
	}
	ready := make(chan line, 1)
	go func() {
		if !a.scanner.Scan() {
			err := a.scanner.Err()
			if err == nil {
				err = io.EOF
			}
			ready <- line{err: err}
			return
		}
		ready <- line{value: strings.TrimSpace(a.scanner.Text())}
	}()
	var value string
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-ready:
		if result.err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", result.err
		}
		value = result.value
	}

	if value == "" {
		return "", fmt.Errorf("输入不能为空")
	}
	return value, nil
}
func (a *answers) decision(ctx context.Context) (string, error) {
	if a.auto {
		_, err := fmt.Fprintln(a.out, "合成案例预设决定：approve（仅模拟，不执行外部发布）")
		return "approve", err
	}
	decision, err := a.text(ctx, "请输入审批决定 approve 或 reject：")
	if err == nil && decision != "approve" && decision != "reject" {
		err = fmt.Errorf("审批决定只能为 approve 或 reject")
	}
	return decision, err
}
func (a *answers) input(ctx context.Context, p *mcpremote.InputRequiredError) (mcp.InputResponseMap, error) {
	responses := mcp.InputResponseMap{}
	for id, request := range p.Requests {
		params, ok := request.(*mcp.ElicitParams)
		if !ok {
			return nil, fmt.Errorf("案例只处理 form elicitation")
		}
		raw, err := json.Marshal(params.RequestedSchema)
		if err != nil {
			return nil, err
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err = json.Unmarshal(raw, &schema); err != nil {
			return nil, err
		}
		content := map[string]any{}
		if _, ok := schema.Properties["decision"]; ok {
			content["decision"], err = a.decision(ctx)
			content["reason"] = "案例操作者决定"
		} else {
			var value string
			value, err = a.text(ctx, "远端 Agent 请求："+params.Message)
			if err == nil {
				raw, err = json.Marshal(value)
				content["input"] = string(raw)
			}
		}
		if err != nil {
			return nil, err
		}
		responses[id] = &mcp.ElicitResult{Action: "accept", Content: content}
	}
	return responses, nil
}

type scenarioResult struct {
	Case   string          `json:"case"`
	TaskID string          `json:"task_id"`
	State  mg.TaskState    `json:"state"`
	Result json.RawMessage `json:"result,omitempty"`
}
type demoClient struct {
	endpoint string
	http     *http.Client
	a2a      *a2aclient.Client
	answers  *answers
	out      io.Writer
}

func runCases(ctx context.Context, endpoint, token, selected string, a *answers) (results []scenarioResult, err error) {
	httpClient, err := authenticatedClient(endpoint, token)
	if err != nil {
		return nil, err
	}
	defer httpClient.CloseIdleConnections()
	client, err := a2aremote.Connect(ctx, endpoint, httpClient)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, client.Destroy()) }()
	d := &demoClient{strings.TrimRight(endpoint, "/"), httpClient, client, a, a.out}
	cases := []struct {
		name string
		run  func(context.Context) (protocol.TaskView, error)
	}{{"cross", d.cross}, {"mcp", d.mcpWorkflow}, {"remote", d.remoteAgent}, {"cancel", d.cancelTask}}
	if selected != "all" {
		found := false
		for _, c := range cases {
			if c.name == selected {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("未知案例 %q", selected)
		}
	}
	results = []scenarioResult{}
	for _, c := range cases {
		if selected != "all" && selected != c.name {
			continue
		}
		if _, err = fmt.Fprintln(d.out, "运行案例："+c.name); err != nil {
			return nil, err
		}
		v, e := c.run(ctx)
		if e != nil {
			return results, fmt.Errorf("案例 %s: %w", c.name, e)
		}
		results = append(results, scenarioResult{c.name, v.ID, v.State, v.Result})
	}
	return results, nil
}
func taskView(v any) (protocol.TaskView, error) {
	var out protocol.TaskView
	raw, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}
func (d *demoClient) connectMCP(ctx context.Context) (*mcp.ClientSession, error) {
	return mcpremote.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: d.endpoint + "/mcp", HTTPClient: d.http})
}
func callTool(ctx context.Context, c *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, error) {
	r, err := c.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	if r.IsError {
		return nil, fmt.Errorf("MCP工具 %s 返回失败: %v", name, r.Content)
	}
	return r, nil
}
func adaptedTool(ctx context.Context, s *mcp.ClientSession, name string, opts ...mcpremote.Option) (interfaces.Tool, error) {
	list, err := mcpremote.Tools(ctx, s, opts...)
	if err != nil {
		return nil, err
	}
	for _, tool := range list {
		if tool.Name() == name {
			return tool, nil
		}
	}
	return nil, fmt.Errorf("未发现 MCP 工具 %s", name)
}
func (d *demoClient) send(ctx context.Context, scenario string) (*api.Task, error) {
	out, err := d.a2a.SendMessage(ctx, &api.SendMessageRequest{Message: &api.Message{ID: uuid.NewString(), Role: api.MessageRoleUser, Parts: api.ContentParts{api.NewDataPart(map[string]any{"scenario": scenario, "message": "示例发布请求"})}}})
	if err != nil {
		return nil, err
	}
	task, ok := out.(*api.Task)
	if !ok {
		return nil, fmt.Errorf("服务没有返回任务")
	}
	return task, nil
}
func (d *demoClient) httpJSON(ctx context.Context, method, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, d.endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	res, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s %s 返回 %d", method, path, res.StatusCode)
	}
	return json.Decode(io.LimitReader(res.Body, 4<<20), out)
}
func (d *demoClient) wait(ctx context.Context, id api.TaskID, desired api.TaskState) (*api.Task, error) {
	for {
		task, err := d.a2a.GetTask(ctx, &api.GetTaskRequest{ID: id})
		if err != nil {
			return nil, err
		}
		if task.Status.State == desired {
			return task, nil
		}
		if task.Status.State == api.TaskStateFailed || task.Status.State == api.TaskStateCanceled || task.Status.State == api.TaskStateCompleted {
			return nil, fmt.Errorf("任务意外结束: %s", task.Status.State)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// A2A 启动 → MCP 输入检查点 → 新连接续接 → HTTP 审批 → A2A/MCP 同任务结果。
func (d *demoClient) cross(ctx context.Context) (resultView protocol.TaskView, err error) {
	var zero protocol.TaskView
	task, err := d.send(ctx, "review")
	if err != nil {
		return zero, err
	}
	s, err := d.connectMCP(ctx)
	if err != nil {
		return zero, err
	}
	tool, err := adaptedTool(ctx, s, "task_wait")
	if err != nil {
		_ = s.Close()
		return zero, err
	}
	_, err = tool.Invoke(ctx, &interfaces.ToolInput{Args: map[string]any{"task_id": task.ID}})
	var pending *mcpremote.InputRequiredError
	if !errors.As(err, &pending) {
		_ = s.Close()
		return zero, fmt.Errorf("预期等待输入，实际: %v", err)
	}
	raw, err := json.Marshal(pending)
	closeErr := s.Close()
	if err != nil || closeErr != nil {
		return zero, errors.Join(err, closeErr)
	}
	var restored mcpremote.InputRequiredError
	if err = json.Unmarshal(raw, &restored); err != nil {
		return zero, err
	}
	s, err = d.connectMCP(ctx)
	if err != nil {
		return zero, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	responses, err := d.answers.input(ctx, &restored)
	if err != nil {
		return zero, err
	}
	_, err = restored.Continue(ctx, s, responses)
	if !errors.As(err, &pending) {
		return zero, fmt.Errorf("预期进入审批等待，实际: %v", err)
	}
	r, err := callTool(ctx, s, "task_get", map[string]any{"task_id": task.ID})
	if err != nil {
		return zero, err
	}
	v, err := taskView(r.StructuredContent)
	if err != nil {
		return zero, err
	}
	if v.ID != string(task.ID) || v.State != mg.WaitingApproval || len(v.Approvals) != 1 {
		return zero, fmt.Errorf("三入口审批身份不一致")
	}
	decision, err := d.answers.decision(ctx)
	if err != nil {
		return zero, err
	}
	ap := v.Approvals[0]
	var response any
	err = d.httpJSON(ctx, "POST", "/v1/approvals/"+url.PathEscape(ap.ID)+"/decision", map[string]any{"expected_revision": ap.Revision, "decision": decision, "reason": "跨入口示例"}, &response)
	if err != nil {
		return zero, err
	}
	final, err := d.wait(ctx, task.ID, api.TaskStateCompleted)
	if err != nil {
		return zero, err
	}
	r, err = callTool(ctx, s, "task_get", map[string]any{"task_id": task.ID})
	if err != nil {
		return zero, err
	}
	v, err = taskView(r.StructuredContent)
	if err != nil {
		return zero, err
	}
	var httpView protocol.TaskView
	if err = d.httpJSON(ctx, "GET", "/v1/tasks/"+url.PathEscape(v.ID), nil, &httpView); err != nil {
		return zero, err
	}
	if v.ID != string(final.ID) || httpView.ID != v.ID || httpView.State != v.State || v.State != mg.Succeeded {
		return zero, fmt.Errorf("最终任务状态不一致")
	}
	return v, nil
}

// 通过远端 MCP 工具回调完成输入和审批两轮，宿主负责展示问题及收集用户决定。
func (d *demoClient) mcpWorkflow(ctx context.Context) (resultView protocol.TaskView, err error) {
	s, err := d.connectMCP(ctx)
	if err != nil {
		return protocol.TaskView{}, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	r, err := callTool(ctx, s, "review_agent", map[string]any{"message_id": uuid.NewString(), "input": map[string]any{"scenario": "review", "message": "MCP发布请求"}})
	if err != nil {
		return protocol.TaskView{}, err
	}
	v, err := taskView(r.StructuredContent)
	if err != nil {
		return v, err
	}
	tool, err := adaptedTool(ctx, s, "task_wait", mcpremote.WithInputHandler(d.answers.input, 4))
	if err != nil {
		return v, err
	}
	out, err := tool.Invoke(ctx, &interfaces.ToolInput{Args: map[string]any{"task_id": v.ID}})
	if err != nil {
		return v, err
	}
	result, ok := out.Result.(*mcp.CallToolResult)
	if !ok {
		return v, fmt.Errorf("MCP没有返回完整工具结果")
	}
	final, err := taskView(result.StructuredContent)
	if err == nil && (final.ID != v.ID || final.State != mg.Succeeded) {
		err = fmt.Errorf("MCP流程未完成")
	}
	return final, err
}

func (d *demoClient) remoteAgent(ctx context.Context) (protocol.TaskView, error) {
	tool, err := a2aremote.Tool(d.a2a, "external_agent", "调用远端 Agent")
	if err != nil {
		return protocol.TaskView{}, err
	}
	out, err := tool.Invoke(ctx, &interfaces.ToolInput{Args: map[string]any{"action": "send", "message_id": uuid.NewString(), "input": map[string]any{"scenario": "echo", "message": "来自调用方的业务请求"}}})
	if err != nil {
		return protocol.TaskView{}, err
	}
	task, ok := out.Result.(*api.Task)
	if !ok || task.Status.State != api.TaskStateCompleted {
		return protocol.TaskView{}, fmt.Errorf("远端 Agent 未完成")
	}
	return taskView(task.Metadata["goagent"])
}

func (d *demoClient) cancelTask(ctx context.Context) (resultView protocol.TaskView, err error) {
	var id api.TaskID
	request := &api.SendMessageRequest{Message: &api.Message{ID: uuid.NewString(), Role: api.MessageRoleUser, Parts: api.ContentParts{api.NewDataPart(map[string]any{"scenario": "cancel", "message": "可取消长任务"})}}}
	for event, err := range d.a2a.SendStreamingMessage(ctx, request) {
		if err != nil {
			return protocol.TaskView{}, err
		}
		task, ok := event.(*api.Task)
		if !ok {
			return protocol.TaskView{}, fmt.Errorf("首个流事件不是Task")
		}
		id = task.ID
		if _, err = fmt.Fprintf(d.out, "收到流式首事件：task_id=%s state=%s\n", id, task.Status.State); err != nil {
			return protocol.TaskView{}, err
		}
		break
	}
	if id == "" {
		return protocol.TaskView{}, fmt.Errorf("流中没有任务")
	}
	// 断开订阅后任务仍在运行，必须显式取消。
	if _, err := d.wait(ctx, id, api.TaskStateWorking); err != nil {
		return protocol.TaskView{}, err
	}
	s, err := d.connectMCP(ctx)
	if err != nil {
		return protocol.TaskView{}, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	r, err := callTool(ctx, s, "task_get", map[string]any{"task_id": id})
	if err != nil {
		return protocol.TaskView{}, err
	}
	v, err := taskView(r.StructuredContent)
	if err != nil {
		return v, err
	}
	if _, err = callTool(ctx, s, "task_cancel", map[string]any{"task_id": id, "message_id": uuid.NewString(), "expected_revision": v.Revision, "reason": "案例显式取消"}); err != nil {
		return v, err
	}
	final, err := d.wait(ctx, id, api.TaskStateCanceled)
	if err != nil {
		return v, err
	}
	return taskView(final.Metadata["goagent"])
}
