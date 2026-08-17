package tool

import (
	"context"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
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

// Bind 把工具绑到指定环境：记忆工具读写该环境快照，其余工具在 Execute 的 context 里带上 env。
// 同名工具后者覆盖前者，避免把全局记忆工具和按环境记忆工具同时交给模型。
func Bind(store *ctxgraph.Store, envID string, tools []Tool) []Tool {
	out := make([]Tool, 0, len(tools))
	indexByName := make(map[string]int, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := tool.Definition().Name
		inner := tool
		if rebound, ok := rebindMemory(store, envID, name); ok {
			inner = rebound
		}
		bound := Tool(boundTool{envID: envID, inner: inner})
		if i, dup := indexByName[name]; dup {
			out[i] = bound
			continue
		}
		indexByName[name] = len(out)
		out = append(out, bound)
	}
	return out
}

func rebindMemory(store *ctxgraph.Store, envID, name string) (Tool, bool) {
	switch name {
	case memoryNeighborsName, memorySubgraphsOfName, memorySourcesOfName, memoryNodesInName, memoryAddToSubgraphName:
	default:
		return nil, false
	}
	for _, tool := range MemoryTools(
		func() ctxgraph.Copy {
			if store == nil {
				return ctxgraph.Copy{}
			}
			return ctxgraph.Copy{Graph: store.Load(envID)}
		},
		func(copy ctxgraph.Copy) {
			if store != nil {
				store.Save(envID, copy.Graph)
			}
		},
	) {
		if tool.Definition().Name == name {
			return tool, true
		}
	}
	return nil, false
}
