package tool

import (
	"context"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
)

type envKey struct{}

// EnvFromContext 返回 Bind 注入的环境 ID；未绑定则为空。
func EnvFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(envKey{}).(string)
	return id
}

// EnvBinder 把工具绑到一个工作区。Bind / BindEnv 只问这个接口，不认工具名。
type EnvBinder interface {
	BindEnv(env.Env) Tool
}

type boundTool struct {
	envID string
	inner Tool
}

func (t boundTool) Definition() Definition {
	return t.inner.Definition()
}

func (t boundTool) Execute(ctx context.Context, call Call) (Output, error) {
	return t.inner.Execute(context.WithValue(ctx, envKey{}, t.envID), call)
}

func (t boundTool) Hidden() bool {
	h, ok := t.inner.(interface{ Hidden() bool })
	return ok && h.Hidden()
}

func unwrapBound(tool Tool) Tool {
	for {
		bound, ok := tool.(boundTool)
		if !ok {
			return tool
		}
		tool = bound.inner
	}
}

type storeView struct {
	store *ctxgraph.Store
	envID string
}

func (v storeView) Snapshot() ctxgraph.Graph {
	if v.store == nil {
		return ctxgraph.Graph{}
	}
	return v.store.Load(v.envID)
}

func (v storeView) Commit(graph ctxgraph.Graph) {
	if v.store != nil {
		v.store.Save(v.envID, graph)
	}
}

// BindEnv 把工具绑到工作区：实现 EnvBinder 的换后端，其余工具在 Execute 的 context 里带上 env ID。
// 同名工具后者覆盖前者。
func BindEnv(e env.Env, tools []Tool) []Tool {
	out := make([]Tool, 0, len(tools))
	indexByName := make(map[string]int, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := tool.Definition().Name
		inner := unwrapBound(tool)
		if binder, ok := inner.(EnvBinder); ok {
			inner = binder.BindEnv(e)
		}
		bound := Tool(boundTool{envID: e.ID, inner: inner})
		if i, dup := indexByName[name]; dup {
			out[i] = bound
			continue
		}
		indexByName[name] = len(out)
		out = append(out, bound)
	}
	return out
}

// Bind 把工具绑到指定环境。内部用 store 适配成 MemoryView 再走 BindEnv。
func Bind(store *ctxgraph.Store, envID string, tools []Tool) []Tool {
	return BindEnv(env.Open(envID, storeView{store: store, envID: envID}), tools)
}
