package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/event"
)

// CurationConfig 控制 compact 之后的全图深度整理触发阈值。
type CurationConfig struct {
	DeepAuditMaxNodes int `yaml:"deep_audit_max_nodes"` // 环境节点数达到该值触发
	DeepAuditMinAdded int `yaml:"deep_audit_min_added"` // 单次 compact 新增达到该值触发
}

const (
	defaultDeepAuditMaxNodes = 64
	defaultDeepAuditMinAdded = 32
)

// Normalized 把零值替换为默认阈值；负值保持原样交由 Validate 拒绝。
func (c CurationConfig) Normalized() CurationConfig {
	out := c
	if out.DeepAuditMaxNodes == 0 {
		out.DeepAuditMaxNodes = defaultDeepAuditMaxNodes
	}
	if out.DeepAuditMinAdded == 0 {
		out.DeepAuditMinAdded = defaultDeepAuditMinAdded
	}
	return out
}

// Validate 拒绝负阈值。
func (c CurationConfig) Validate() error {
	if c.DeepAuditMaxNodes < 0 {
		return fmt.Errorf("memory.curation.deep_audit_max_nodes must not be negative")
	}
	if c.DeepAuditMinAdded < 0 {
		return fmt.Errorf("memory.curation.deep_audit_min_added must not be negative")
	}
	return nil
}

// nodeCount 返回当前绑定记忆图的节点数；未绑定时返回 -1。
func (l *Loop) nodeCount() int {
	memory := l.memoryView()
	if memory == nil {
		return -1
	}
	return len(memory.Snapshot().Nodes)
}

func (l *Loop) memoryView() env.MemoryView {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	memory := l.memory
	l.mu.Unlock()
	return memory
}

// organizerLoop 返回本循环通过 organize_subgraph 关联的整理 Agent。
func (l *Loop) organizerLoop() *Loop {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	tool := l.tools[organizeSubgraphToolName]
	l.mu.Unlock()
	return organizerFromTool(tool)
}

// compactAndMaybeCurate 执行隐藏 compact，再按阈值判断是否触发全图深度整理。
func compactAndMaybeCurate(ctx context.Context, loop *Loop, keep int) error {
	before := loop.nodeCount()
	if err := execHiddenErr(loop, ctx, compactMemoryToolName, keepRecentArgs(keep)); err != nil {
		return err
	}
	maybeDeepCuration(ctx, loop, before)
	return nil
}

// maybeDeepCuration 在节点总量或单次新增跨过阈值时，让整理 Agent 审核有界快照。
// 整理是尽力而为：失败只进事件流，不让外层钩子失败。
func maybeDeepCuration(ctx context.Context, loop *Loop, before int) {
	if err := ctx.Err(); err != nil {
		return
	}
	config := loop.deepCurationConfig()
	organizer := loop.organizerLoop()
	memory := loop.memoryView()
	if organizer == nil || memory == nil || organizer.isRunning() {
		return
	}
	snapshot := memory.Snapshot()
	if !shouldDeepCurate(config, len(snapshot.Nodes), len(snapshot.Nodes)-before) {
		return
	}
	runDeepCuration(ctx, organizer, snapshot)
}

// shouldDeepCurate 判定是否触发全图深度整理：总量或单次新增任一跨过阈值。
func shouldDeepCurate(config CurationConfig, total, added int) bool {
	return total >= config.DeepAuditMaxNodes || added >= config.DeepAuditMinAdded
}

func (l *Loop) deepCurationConfig() CurationConfig {
	if l == nil {
		return CurationConfig{}.Normalized()
	}
	l.mu.Lock()
	config := l.curation
	l.mu.Unlock()
	return config.Normalized()
}

func (l *Loop) isRunning() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

// RunDeepCuration 让整理 Agent 审核一份有界图快照。
// 生产路径由 compact 后的阈值触发（maybeDeepCuration）；导出是为了让评测
// 直接覆盖深度整理模式，而不是在评测里另写一份请求构造。
func RunDeepCuration(ctx context.Context, organizer *Loop, snapshot ctxgraph.Graph) {
	if organizer == nil {
		return
	}
	runDeepCuration(ctx, organizer, snapshot)
}

func runDeepCuration(ctx context.Context, organizer *Loop, snapshot ctxgraph.Graph) {
	const operation = "curation_audit"
	started := time.Now()
	organizer.publish(ctx, event.MemoryStart(organizer.agentID, operation, ""))
	_, err := organizer.Ask(ctx, deepCurationQuery(snapshot))
	organizer.publish(ctx, event.MemoryOrganized(organizer.agentID, operation, "", started, len(snapshot.Nodes), -1, err))
}

// deepCurationQuery 构造有界审核请求：逐节点列出 ID、kind/status、创建者与陈述。
func deepCurationQuery(snapshot ctxgraph.Graph) string {
	var b strings.Builder
	b.WriteString("深度整理请求：只审核下列实际提供的有界节点；输入可能因大小限制省略中段，不得声称看到全图。规则见系统提示（证据准入、矛盾仲裁、去重和保护层）。")
	b.WriteString("所有修改用 memory_apply 提交，每条带 reason；不确定的节点不要动。完成后简短确认操作摘要。\n\n")
	b.WriteString("节点清单（ID [kind/status] 创建者）：\n")
	for _, node := range snapshot.Nodes {
		creator := node.CreatorAgentID
		if creator == "" {
			creator = "unknown"
		}
		statement := clipMiddle(node.Statement, maxOrganizePromptBytes/8)
		fmt.Fprintf(&b, "- %s [%s/%s]（%s）%s\n", node.ID, node.Kind, node.Status, creator, statement)
	}
	return clipMiddle(b.String(), maxOrganizePromptBytes)
}
