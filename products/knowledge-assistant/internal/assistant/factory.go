package assistant

import (
	"context"

	"github.com/kart-io/goagent-contrib/retrieval"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/llm"
	mg "github.com/kart-io/goagent/management"
	"github.com/kart-io/goagent/management/adapters"
	"github.com/kart-io/goagent/memory"

	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/docs"
)

// Deps 是 Factory 构建 Agent 所需的进程级依赖。
type Deps struct {
	Stores    *Stores
	Model     llm.ToolStreamClient
	Index     *docs.Index
	CorpusDir string
	Embedder  retrieval.Embedder
}

// Factory 返回 runner_kind "knowledge" 的管理面 Factory。
func Factory(d Deps) mg.Factory {
	return factory{inner: adapters.NativeFactory{
		Check: func(_ context.Context, v mg.Version) error {
			if v.ImplementationVersion != ImplementationVersion {
				return agentErrors.ErrManagementUnsupported
			}
			return nil
		},
		BuildAgent: d.buildAgent,
	}}
}

type factory struct{ inner adapters.NativeFactory }

func (f factory) Validate(ctx context.Context, v mg.Version) (mg.Capabilities, error) {
	return f.inner.Validate(ctx, v)
}

func (f factory) Build(ctx context.Context, v mg.Version, r mg.Resources) (mg.Binding, error) {
	b, err := f.inner.Build(ctx, v, r)
	if err != nil || b.Runtime == nil {
		return b, err
	}
	b.Runtime = nsRuntime{inner: b.Runtime}
	return b, nil
}

// nsRuntime 把任务租户绑定为记忆命名空间：management 的原生适配器不做这件事（FINDINGS F1），
// Inspect 回放也须绑定，否则长期记忆检测到命名空间变化而失败。
type nsRuntime struct{ inner mg.Runtime }

func (n nsRuntime) Run(ctx context.Context, spec mg.RunSpec) (mg.RunResult, error) {
	return n.inner.Run(memory.WithNamespace(ctx, spec.Task.TenantID), spec)
}

func (n nsRuntime) Inspect(ctx context.Context, spec mg.RunSpec) (mg.RunEvidence, error) {
	return n.inner.Inspect(memory.WithNamespace(ctx, spec.Task.TenantID), spec)
}
