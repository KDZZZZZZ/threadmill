package context

import "sync"

// Copy 是某个 Agent 持有的全局图副本。独立 Agent 只应持有一份。
type Copy struct {
	AgentID string
	Graph   Graph
}

var (
	globalMu    sync.Mutex
	globalGraph Graph
)

// Clone 返回打上 agentID 的全局图副本。所有读写都必须在这份副本上进行。
func Clone(agentID string) Copy {
	globalMu.Lock()
	defer globalMu.Unlock()
	return Copy{
		AgentID: agentID,
		Graph:   globalGraph.Clone(),
	}
}

// Update 用副本替换全局记忆图。传入的图会被再拷贝，调用方可继续改自己的副本。
func Update(copy Copy) {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalGraph = copy.Graph.Clone()
}
