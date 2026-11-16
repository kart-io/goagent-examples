package assistant

import (
	"context"
	"encoding/json"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/interfaces"
	mg "github.com/kart-io/goagent/management"
	"github.com/kart-io/goagent/tools"

	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/docs"
)

const (
	RunnerKind            = "knowledge"
	ImplementationVersion = "ka-1"
	AgentName             = "knowledge-assistant"
	AgentVersion          = "v1"
)

const systemPrompt = `你是团队知识助手，始终用中文回答。
回答依据检索到的资料时，逐条写出每份所依据资料第一行"来源："后面的路径。
资料和记忆中都没有证据时，直接说明不知道，不要编造。`

const noArgs = `{"type":"object","properties":{},"additionalProperties":false}`

// approvalTool 是执行前须人工审批的工具；结果文本避免出现"拒绝"，脚本模型据此识别审批被拒。
type approvalTool struct {
	name, label, desc, feedback string // label 是写入 ka_tool_runs.tool 的短名
	run                         func(ctx context.Context, in *interfaces.ToolInput) (string, error)
}

func (d Deps) approvalTools() []approvalTool {
	return []approvalTool{
		{"purge_my_memory", "purge", "清空当前用户的全部长期记忆，不可恢复。", "清空个人记忆需要人工审批",
			func(ctx context.Context, _ *interfaces.ToolInput) (string, error) {
				if err := d.Stores.Memory.Purge(ctx); err != nil {
					return "", err
				}
				return "已清空当前用户的全部记忆", nil
			}},
		{"rebuild_docs_index", "rebuild", "重新采集语料并重建文档索引，返回新集合名。", "重建文档索引需要人工审批",
			func(ctx context.Context, in *interfaces.ToolInput) (string, error) {
				chunks, err := docs.Collect(d.CorpusDir)
				if err != nil {
					return "", err
				}
				return d.Index.Rebuild(ctx, in.ExecutionKey, chunks)
			}},
	}
}

func (d Deps) buildAgent(ctx context.Context, _ mg.Version, r mg.Resources, ap tools.Approver) (*builder.SimpleAgentBuilder, func(context.Context) error, error) {
	retriever, err := d.Index.Retriever(ctx, 4)
	if err != nil {
		return nil, nil, err
	}
	tenant := r.Task.TenantID
	registry := tools.NewRegistry()
	for _, t := range d.Stores.Memory.Tools() {
		if err := registry.Register(t); err != nil {
			return nil, nil, err
		}
	}
	feedback := map[string]string{}
	for _, t := range d.approvalTools() {
		feedback[t.name] = t.feedback
		err := registry.Register(tools.NewBaseTool(t.name, t.desc, noArgs, func(ctx context.Context, in *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
			if _, err := d.Stores.Pool.Exec(ctx, `INSERT INTO ka_tool_runs(execution_key, tool, tenant) VALUES($1, $2, $3)`,
				in.ExecutionKey, t.label, tenant); err != nil {
				return &interfaces.ToolOutput{Success: false}, err
			}
			res, err := t.run(ctx, in)
			return &interfaces.ToolOutput{Success: err == nil, Result: res}, err
		}))
		if err != nil {
			return nil, nil, err
		}
	}
	approvalHook := func(_ context.Context, c *tools.Call) (tools.PreDecision, error) {
		if fb, ok := feedback[c.Tool]; ok {
			return tools.PreDecision{Kind: tools.PreAsk, Feedback: fb}, nil
		}
		return tools.PreDecision{Kind: tools.PreAllow}, nil
	}
	dispatcher, err := tools.NewDispatcher(registry, tools.DispatchConfig{
		Pipeline: &tools.Pipeline{Pre: []tools.PreHook{approvalHook}, Approver: ap},
	})
	if err != nil {
		return nil, nil, err
	}
	return builder.NewSimpleBuilder(d.Model).
		WithSystemPrompt(systemPrompt).
		WithRetriever(retriever, builder.RetrievalConfig{MaxDocuments: 4}).
		WithLongTermMemory(d.Stores.Memory).
		WithModelCache(d.Stores.Cache).
		WithTools(dispatcher.Tools()...).
		WithMaxIterations(6), nil, nil
}

// EnsureAgent 为 a 的租户注册并发布知识助手；固定幂等键让重复调用返回同一 Agent，幂等记录按租户隔离。
func EnsureAgent(ctx context.Context, s *mg.Service, a mg.Actor) (mg.Agent, error) {
	agent, err := s.RegisterAgent(ctx, a, mg.RegisterAgentRequest{IdempotencyKey: "ka-register", Name: AgentName})
	if err != nil {
		return mg.Agent{}, err
	}
	if _, err := s.SaveVersion(ctx, a, mg.SaveVersionRequest{
		IdempotencyKey:        "ka-save-" + AgentVersion,
		AgentID:               agent.ID,
		Version:               AgentVersion,
		RunnerKind:            RunnerKind,
		ImplementationVersion: ImplementationVersion,
		Config:                json.RawMessage(`{}`),
	}); err != nil {
		return mg.Agent{}, err
	}
	return s.PublishVersion(ctx, a, mg.PublishRequest{
		IdempotencyKey:   "ka-publish-" + AgentVersion,
		AgentID:          agent.ID,
		Version:          AgentVersion,
		ExpectedRevision: agent.Revision,
	})
}
