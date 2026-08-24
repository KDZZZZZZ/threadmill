// Command tmfleet 用 mock provider 驱动完整的 coordination→Assemble→task→role
// 编排链路做压测：不调用真实模型，度量编排层（图调度、记忆 fork/merge、
// 进度/checkpoint 落盘、事件总线、钩子）在 N 个并行 spawn 下的扩展性。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	"github.com/KDZZZZZZ/threadmill/internal/exec"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func main() {
	spawns := flag.Int("spawns", 100, "并行 spawn 任务数")
	delay := flag.Duration("model-delay", 100*time.Millisecond, "每次 mock 模型调用的延迟")
	slots := flag.Int("slots", 32, "执行槽位")
	files := flag.Int("files", 1000, "基线仓文件数")
	timeout := flag.Duration("timeout", 20*time.Minute, "总超时")
	flag.Parse()

	dir, err := os.MkdirTemp("", "tmfleet-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tmfleet:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	if err := run(*spawns, *delay, *slots, *files, *timeout, dir); err != nil {
		fmt.Fprintln(os.Stderr, "tmfleet:", err)
		os.Exit(1)
	}
}

func run(spawns int, delay time.Duration, slots, files int, timeout time.Duration, dir string) error {
	// 内置默认配置（提示词、角色装配）。
	cfg, err := provider.LoadRuntimeConfig(dir, "")
	if err != nil {
		return err
	}

	repo, err := makeFixture(dir, files)
	if err != nil {
		return err
	}
	filesStore, err := vfs.NewPersistentStore(repo, filepath.Join(dir, "live"))
	if err != nil {
		return err
	}
	sched := exec.New(exec.Config{
		Slots:           slots,
		Timeout:         30 * time.Second,
		ExternalSandbox: true,
		HeavyThreshold:  24 * time.Hour, // 压测关闭车道干扰
	})
	memory := ctxgraph.NewStore()

	graph, err := coordination.OpenGraph(filepath.Join(dir, "graph.json"))
	if err != nil {
		return err
	}
	progress, err := coordination.NewDirProgressStore(filepath.Join(dir, "progress"))
	if err != nil {
		return err
	}
	graph.SetProgressStore(progress)
	checkpoints, err := agent.NewDirCheckpointStore(filepath.Join(dir, "checkpoints"))
	if err != nil {
		return err
	}

	root := graph.AddTask()
	for i := range spawns {
		if _, err := graph.Spawn(root.Planner.ID, root.Verifier.ID); err != nil {
			return fmt.Errorf("spawn %d: %w", i, err)
		}
	}

	mock := newFleetProvider(delay)
	stores := coordination.Stores{Memory: memory, Files: filesStore, Exec: sched}
	overlay := agent.FileOverlay{
		Prompts:    cfg.Prompts,
		Curation:   cfg.Memory.Curation,
		NamedTools: graph.HelpTools(nil),
	}
	assemble := coordination.Assemble(
		stores, mock, cfg.Agents, nil, cfg.LLM.ContextWindow, checkpoints,
		overlay,
	)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	started := time.Now()
	out, err := graph.Run(ctx, root.ID, "fleet 压测根任务：验证编排层在并行 spawn 下的扩展性", stores, assemble)
	wall := time.Since(started)

	fmt.Printf("tmfleet: spawns=%d delay=%s wall=%s err=%v\n", spawns, delay, wall.Truncate(time.Millisecond), err)
	if strings.TrimSpace(out) != "" {
		fmt.Printf("root verifier tail: %s\n", tail(out, 200))
	}
	mock.report()
	vs := filesStore.Stats()
	es := sched.Stats()
	fmt.Printf(
		"vfs: envs=%d materialize=%d(%s) absorb_fast=%d upper=%d entries=%d fallbacks=%d errors=%d(%s) absorb_scans=%d(%s) absorb_errors=%d absorb_peak=%d/%d absorb_wait=%s overlay_files=%d\n",
		vs.Environments,
		vs.MaterializeCopies,
		vs.MaterializeCopyDuration.Truncate(time.Millisecond),
		vs.AbsorbFastPaths,
		vs.AbsorbUpperAttempts,
		vs.AbsorbUpperEntries,
		vs.AbsorbUpperFallbacks,
		vs.AbsorbUpperErrors,
		vs.AbsorbUpperDuration.Truncate(time.Millisecond),
		vs.AbsorbScans,
		vs.AbsorbScanDuration.Truncate(time.Millisecond),
		vs.AbsorbScanErrors,
		vs.AbsorbPeakActive,
		vs.AbsorbCapacity,
		vs.AbsorbWaitDuration.Truncate(time.Millisecond),
		vs.OverlayFiles,
	)
	fmt.Printf("exec: requests=%d completed=%d queued_peak=%d wait=%s run=%s\n",
		es.Requests, es.Completed, es.PeakQueued, es.WaitDuration.Truncate(time.Millisecond), es.RunDuration.Truncate(time.Millisecond))
	fmt.Printf("rss_peak=%s\n", peakRSS())
	return err
}

// fleetProvider 按角色脚本返回模型消息；用系统提示词关键词区分角色。
type fleetProvider struct {
	delay time.Duration
	mu    sync.Mutex
	seq   int
	calls map[string]*atomic.Int64
}

func newFleetProvider(delay time.Duration) *fleetProvider {
	return &fleetProvider{delay: delay, calls: map[string]*atomic.Int64{
		"planner": {}, "executor": {}, "verifier": {}, "organizer": {}, "compact": {},
	}}
}

func (p *fleetProvider) Generate(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
	role := detectRole(request.SystemPrompt)
	p.calls[role].Add(1)
	select {
	case <-ctx.Done():
		return agent.AssistantMessage{}, ctx.Err()
	case <-time.After(p.delay):
	}
	p.mu.Lock()
	p.seq++
	seq := p.seq
	p.mu.Unlock()

	if role == "compact" {
		return agent.AssistantMessage{Content: `{"nodes":[{"kind":"fact","statement":"fleet 压测压缩节点 pytest 退出码 0","status":"accepted","subgraph_ids":[]}]}`}, nil
	}
	if role == "organizer" {
		return agent.AssistantMessage{Content: "未找到需要加入的相关节点"}, nil
	}

	turn := 0
	for _, message := range request.Messages {
		if message.Role == agent.RoleAssistant {
			turn++
		}
	}
	switch role {
	case "planner":
		if turn == 0 {
			return toolCall(seq, "read", `{"path":"src/pkg-0/file_0.txt"}`), nil
		}
		return agent.AssistantMessage{Content: "计划：完成 fleet 目标；验收矩阵 1 项；门禁 echo 退出码 0。"}, nil
	case "executor":
		switch turn {
		case 0:
			return toolCall(seq, "write", `{"path":"fleet_edit.txt","content":"fleet 交付改动"}`), nil
		case 1:
			return toolCall(seq, "bash", `{"command":"echo fleet-gate-ok"}`), nil
		}
		return agent.AssistantMessage{Content: "已完成：fleet_edit.txt 写入，echo fleet-gate-ok 退出码 0。git diff 显示 1 个文件变更。"}, nil
	default: // verifier
		if turn == 0 {
			return toolCall(seq, "bash", `{"command":"echo verifier-gate"}`), nil
		}
		return agent.AssistantMessage{Content: "结论: PASS\n门禁证据：\necho verifier-gate 退出码 0"}, nil
	}
}

func toolCall(seq int, name, args string) agent.AssistantMessage {
	return agent.AssistantMessage{
		ToolCalls: []agenttool.Call{{ID: fmt.Sprintf("fleet-call-%d", seq), Name: name, Arguments: json.RawMessage(args)}},
	}
}

func detectRole(systemPrompt string) string {
	switch {
	case strings.Contains(systemPrompt, "记忆整理器"):
		return "compact"
	case strings.Contains(systemPrompt, "记忆子图整理"):
		return "organizer"
	case strings.Contains(systemPrompt, "规划 Agent"):
		return "planner"
	case strings.Contains(systemPrompt, "执行 Agent"):
		return "executor"
	case strings.Contains(systemPrompt, "核验 Agent"):
		return "verifier"
	}
	return "verifier"
}

func (p *fleetProvider) report() {
	total := int64(0)
	for role, counter := range p.calls {
		total += counter.Load()
		fmt.Printf("model[%s]=%d ", role, counter.Load())
	}
	fmt.Printf("model[total]=%d\n", total)
}

func makeFixture(root string, files int) (string, error) {
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "src", "pkg-0"), 0o750); err != nil {
		return "", err
	}
	data := []byte(strings.Repeat("x", 1024))
	for i := 0; i < files; i++ {
		dir := filepath.Join(repo, "src", fmt.Sprintf("pkg-%d", i%64))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file_%d.txt", i)), data, 0o640); err != nil {
			return "", err
		}
	}
	return repo, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func peakRSS() string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "?"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:"))
		}
	}
	return "?"
}
