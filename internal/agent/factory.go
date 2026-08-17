package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// DefaultSystemPrompt 是 Agent 默认使用的系统提示词。
const DefaultSystemPrompt = `你是 Threadmill，一个通过 ReAct 循环完成任务的 AI Agent。

根据用户请求决定下一步行动。需要读取信息或执行操作时，调用可用工具；不要编造工具结果。收到工具结果后继续处理，直到可以直接回答用户。`

// SubgraphOrganizerPrompt 是根据查询整理对应记忆子图的系统提示词。
const SubgraphOrganizerPrompt = `你是记忆子图整理 Agent。根据用户查询，使用记忆图工具找出相关节点，并把它们加入用户消息指定的目标子图。

规则：
- 只使用工具返回的节点 ID，不要编造。
- 用 memory_add_to_subgraph 把选中节点加入目标子图 ID，不要更换 ID。
- 完成后用简短文字确认即可。`

const subgraphOrganizerID = "subgraph-organizer"
const organizeSubgraphToolName = "organize_subgraph"

const (
	memoryNeighborsToolName       = "memory_neighbors"
	memorySubgraphsOfToolName     = "memory_subgraphs_of"
	memorySourcesOfToolName       = "memory_sources_of"
	memoryNodesInToolName         = "memory_nodes_in"
	memoryAddToSubgraphToolName   = "memory_add_to_subgraph"
	memoryDropFromContextToolName = "memory_drop_from_context"

	fileReadToolName  = "read"
	fileWriteToolName = "write"
	fileEditToolName  = "edit"
	fileLsToolName    = "ls"
	fileGrepToolName  = "grep"
	fileFindToolName  = "find"
	bashToolName      = "bash"

	hookInjectSubscribedMemory      = "inject_subscribed_memory"
	hookCompactOnOverflow           = "compact_on_overflow"
	hookCommitTailOnTurnEnd         = "commit_tail_on_turn_end"
	hookRemindDropContextOnPressure = "remind_drop_context_on_pressure"
)

const (
	plannerID  = "planner"
	executorID = "executor"
	verifierID = "verifier"
)

// PlannerPrompt 是规划 Agent 的系统提示词。
const PlannerPrompt = `你是规划 Agent。根据用户目标和订阅记忆，产出可执行的分步计划。

规则：
- 只规划，不要执行计划中的操作。
- 每一步具体、可独立执行，按合理顺序排列。
- 不要编造工具结果；需要读信息时才调用可用工具。`

// ExecutorPrompt 是执行 Agent 的系统提示词。
const ExecutorPrompt = `你是执行 Agent。按给定计划逐步完成当前任务。

规则：
- 严格按计划执行，不要擅自改写目标。
- 需要操作时调用可用工具；不要编造工具结果。
- 完成后用简短文字报告这一步的结果。`

// VerifierPrompt 是核验 Agent 的系统提示词。
const VerifierPrompt = `你是核验 Agent。根据目标、计划和执行结果，判断任务是否完成。

规则：
- 只核验，不要继续执行计划，不要改写计划。
- 明确给出通过或不通过，并列出依据。
- 不要编造未出现在记忆或对话里的事实。`

var nextQuerySubgraphID atomic.Uint64

var knownFileTools = map[string]struct{}{
	organizeSubgraphToolName:      {},
	memoryNeighborsToolName:       {},
	memorySubgraphsOfToolName:     {},
	memorySourcesOfToolName:       {},
	memoryNodesInToolName:         {},
	memoryAddToSubgraphToolName:   {},
	memoryDropFromContextToolName: {},
	fileReadToolName:              {},
	fileWriteToolName:             {},
	fileEditToolName:              {},
	fileLsToolName:                {},
	fileGrepToolName:              {},
	fileFindToolName:              {},
	bashToolName:                  {},
}

var knownFileHooks = map[string]struct{}{
	hookInjectSubscribedMemory:      {},
	hookCompactOnOverflow:           {},
	hookCommitTailOnTurnEnd:         {},
	hookRemindDropContextOnPressure: {},
}

// FileAgent 是 threadmill.yaml 里单个 Agent 的配置。
type FileAgent struct {
	ID           string   `yaml:"id"`
	MaxSteps     int      `yaml:"max_steps"`
	SystemPrompt string   `yaml:"system_prompt"`
	Tools        []string `yaml:"tools"`
	Hooks        []string `yaml:"hooks"`
}

// FileAgents 是 threadmill.yaml 里四个内置 Agent 的配置。
type FileAgents struct {
	Planner           FileAgent `yaml:"planner"`
	Executor          FileAgent `yaml:"executor"`
	Verifier          FileAgent `yaml:"verifier"`
	SubgraphOrganizer FileAgent `yaml:"subgraph_organizer"`
}

// Team 是由 yaml 配置装配出的规划、执行、核验和子图整理 Agent。
type Team struct {
	Planner   *Loop
	Executor  *Loop
	Verifier  *Loop
	Organizer *Loop
}

// Validate 拒绝空白 ID、负的步数，以及未知或重复的 tool/hook 名。
func (agents FileAgents) Validate() error {
	for _, item := range []struct {
		name string
		role FileAgent
	}{
		{"planner", agents.Planner},
		{"executor", agents.Executor},
		{"verifier", agents.Verifier},
		{"subgraph_organizer", agents.SubgraphOrganizer},
	} {
		if err := item.role.validate(item.name); err != nil {
			return err
		}
	}
	return nil
}

func (role FileAgent) validate(name string) error {
	if strings.TrimSpace(role.ID) != role.ID {
		return fmt.Errorf("agents.%s.id must not have surrounding whitespace", name)
	}
	if role.MaxSteps < 0 {
		return fmt.Errorf("agents.%s.max_steps must not be negative", name)
	}
	if err := validatePluginNames(name, "tools", role.Tools, knownFileTools); err != nil {
		return err
	}
	if err := validatePluginNames(name, "hooks", role.Hooks, knownFileHooks); err != nil {
		return err
	}
	return nil
}

func (role FileAgent) loopConfig(provider Provider, tools []agenttool.Tool) Config {
	return Config{
		AgentID:      role.ID,
		Provider:     provider,
		Tools:        tools,
		MaxSteps:     role.MaxSteps,
		SystemPrompt: role.SystemPrompt,
	}
}

// agentConfig 是 Factory 在 Run 前确定的静态 Agent 配置。
type agentConfig struct {
	systemPrompt string
}

// newAgentConfig 创建当前 Agent 的静态配置快照。
func newAgentConfig() agentConfig {
	return agentConfig{systemPrompt: DefaultSystemPrompt}
}

type requesterBinder interface {
	BindRequester(*Loop)
}

type organizeSubgraphTool struct {
	organizer *Loop
	requester *Loop
	memory    env.MemoryView
}

var _ agenttool.Tool = (*organizeSubgraphTool)(nil)
var _ agenttool.EnvBinder = (*organizeSubgraphTool)(nil)
var _ requesterBinder = (*organizeSubgraphTool)(nil)

// NewSubgraphOrganizer 创建只整理记忆子图的 Agent，并注册记忆图工具。
func NewSubgraphOrganizer(config Config) (*Loop, error) {
	extra := config.Tools
	config.Tools = nil
	if config.AgentID == "" {
		config.AgentID = subgraphOrganizerID
	}
	if config.SystemPrompt == "" {
		config.SystemPrompt = SubgraphOrganizerPrompt
	}

	loop, err := NewLoop(config)
	if err != nil {
		return nil, err
	}

	tools, definitions, err := prepareTools(append(
		agenttool.MemoryTools(nil, nil),
		extra...,
	))
	if err != nil {
		return nil, err
	}
	loop.tools = tools
	loop.definitions = definitions
	return loop, nil
}

// NewPlanner 创建规划 Agent，挂上记忆 hook，并注册向子图整理 Agent 发 query 的工具。
func NewPlanner(config Config) (*Loop, error) {
	return newMemoryLoop(
		config,
		plannerID,
		PlannerPrompt,
		nil,
	)
}

// NewExecutor 创建执行 Agent，挂上记忆 hook，并注册向子图整理 Agent 发 query 的工具。
func NewExecutor(config Config) (*Loop, error) {
	return newMemoryLoop(
		config,
		executorID,
		ExecutorPrompt,
		nil,
	)
}

// NewVerifier 创建核验 Agent，挂上记忆 hook，并注册向子图整理 Agent 发 query 的工具。
func NewVerifier(config Config) (*Loop, error) {
	return newMemoryLoop(
		config,
		verifierID,
		VerifierPrompt,
		nil,
	)
}

// NewTeam 按 yaml 配置装配四个 Agent，上下文窗口来自模型，规划/执行/核验共用同一个整理 Agent。
func NewTeam(
	provider Provider,
	contextWindow int,
	agents FileAgents,
	extra []agenttool.Tool,
) (*Team, error) {
	if err := agents.Validate(); err != nil {
		return nil, err
	}

	organizerCfg := agents.SubgraphOrganizer.loopConfig(provider, nil)
	organizerCfg.ContextWindow = contextWindow
	organizer, err := newFileLoop(
		organizerCfg,
		agents.SubgraphOrganizer,
		subgraphOrganizerID,
		SubgraphOrganizerPrompt,
		nil,
	)
	if err != nil {
		return nil, err
	}

	plannerCfg := agents.Planner.loopConfig(provider, extra)
	plannerCfg.ContextWindow = contextWindow
	planner, err := newFileLoop(
		plannerCfg,
		agents.Planner,
		plannerID,
		PlannerPrompt,
		organizer,
	)
	if err != nil {
		return nil, err
	}

	executorCfg := agents.Executor.loopConfig(provider, extra)
	executorCfg.ContextWindow = contextWindow
	executor, err := newFileLoop(
		executorCfg,
		agents.Executor,
		executorID,
		ExecutorPrompt,
		organizer,
	)
	if err != nil {
		return nil, err
	}

	verifierCfg := agents.Verifier.loopConfig(provider, extra)
	verifierCfg.ContextWindow = contextWindow
	verifier, err := newFileLoop(
		verifierCfg,
		agents.Verifier,
		verifierID,
		VerifierPrompt,
		organizer,
	)
	if err != nil {
		return nil, err
	}

	return &Team{
		Planner:   planner,
		Executor:  executor,
		Verifier:  verifier,
		Organizer: organizer,
	}, nil
}

// BindCheckpoints 把进行中 ReAct 快照绑到 task 角色 ID 上，避免多个 task 互相覆盖。
func (t *Team) BindCheckpoints(store CheckpointStore, taskID string) {
	bind := func(loop *Loop, role string) {
		if loop == nil {
			return
		}
		if taskID != "" {
			loop.agentID = taskID + ":" + role
		}
		loop.checkpoints = store
	}
	bind(t.Planner, plannerID)
	bind(t.Executor, executorID)
	bind(t.Verifier, verifierID)
	bind(t.Organizer, subgraphOrganizerID)
}

func withFileDefaults(config Config, defaultID, defaultPrompt string) Config {
	if config.AgentID == "" {
		config.AgentID = defaultID
	}
	if config.SystemPrompt == "" {
		config.SystemPrompt = defaultPrompt
	}
	return config
}

// Bind 把 yaml 装好的工具接到工作区；同一 task 的角色共用这份 env。
func (t *Team) Bind(e env.Env) error {
	for _, loop := range []*Loop{t.Planner, t.Executor, t.Verifier, t.Organizer} {
		if err := loop.Bind(e); err != nil {
			return err
		}
	}
	return nil
}

// Bind 把循环里的工具接到工作区，包括 NewPlanner 自行创建的整理 Agent。
func (l *Loop) Bind(e env.Env) error {
	if l == nil {
		return nil
	}
	return bindLoopTools(l, e)
}

func bindLoopTools(loop *Loop, e env.Env) error {
	if loop == nil {
		return nil
	}
	loop.mu.Lock()
	if tool, ok := loop.tools[organizeSubgraphToolName].(*organizeSubgraphTool); ok && tool.organizer != nil {
		organizer := tool.organizer
		loop.mu.Unlock()
		if err := bindLoopTools(organizer, e); err != nil {
			return err
		}
		loop.mu.Lock()
	}
	listed := listedToolsLocked(loop)
	loop.mu.Unlock()

	tools, definitions, err := prepareTools(agenttool.BindEnv(e, listed))
	if err != nil {
		return err
	}

	loop.mu.Lock()
	loop.tools = tools
	loop.definitions = definitions
	loop.mu.Unlock()
	return nil
}

func listedToolsLocked(loop *Loop) []agenttool.Tool {
	listed := make([]agenttool.Tool, 0, len(loop.tools))
	seen := make(map[string]struct{}, len(loop.tools))
	for _, def := range loop.definitions {
		tool, ok := loop.tools[def.Name]
		if !ok {
			continue
		}
		listed = append(listed, tool)
		seen[def.Name] = struct{}{}
	}
	for name, tool := range loop.tools {
		if _, ok := seen[name]; ok {
			continue
		}
		listed = append(listed, tool)
	}
	return listed
}

func registerTools(loop *Loop, extra []agenttool.Tool) error {
	if loop == nil || len(extra) == 0 {
		return nil
	}
	listed := listedToolsLocked(loop)
	seen := make(map[string]struct{}, len(listed))
	for _, tool := range listed {
		seen[tool.Definition().Name] = struct{}{}
	}
	for _, tool := range extra {
		if tool == nil {
			continue
		}
		name := tool.Definition().Name
		if _, ok := seen[name]; ok {
			continue
		}
		listed = append(listed, tool)
		seen[name] = struct{}{}
	}
	tools, definitions, err := prepareTools(listed)
	if err != nil {
		return err
	}
	loop.tools = tools
	loop.definitions = definitions
	return nil
}

func newFileLoop(
	config Config,
	file FileAgent,
	defaultID string,
	defaultPrompt string,
	organizer *Loop,
) (*Loop, error) {
	config = withFileDefaults(config, defaultID, defaultPrompt)
	loop, err := NewLoop(config)
	if err != nil {
		return nil, err
	}
	if err := installNamedPlugins(loop, file.Tools, file.Hooks, organizer); err != nil {
		return nil, fmt.Errorf("install %s plugins: %w", loop.agentID, err)
	}
	return loop, nil
}

func validatePluginNames(role, field string, names []string, known map[string]struct{}) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("agents.%s.%s must not contain an empty name", role, field)
		}
		if strings.TrimSpace(name) != name {
			return fmt.Errorf("agents.%s.%s must not have surrounding whitespace", role, field)
		}
		if _, ok := known[name]; !ok {
			return fmt.Errorf("agents.%s.%s: unknown %q", role, field, name)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("agents.%s.%s: duplicate %q", role, field, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func installNamedPlugins(
	loop *Loop,
	toolNames []string,
	hookNames []string,
	organizer *Loop,
) error {
	if len(toolNames) > 0 {
		named, err := toolsFromNames(toolNames, loop, organizer)
		if err != nil {
			return err
		}
		if err := registerTools(loop, named); err != nil {
			return err
		}
		bindRequesters(loop, loop.tools)
	}

	hooks, hidden, err := hooksFromNames(hookNames, loop)
	if err != nil {
		return err
	}
	if err := registerTools(loop, hidden); err != nil {
		return err
	}
	return loop.AddHooks(hooks)
}

func toolsFromNames(
	names []string,
	loop *Loop,
	organizer *Loop,
) ([]agenttool.Tool, error) {
	var memory map[string]agenttool.Tool
	var files map[string]agenttool.Tool
	out := make([]agenttool.Tool, 0, len(names))
	for _, name := range names {
		switch name {
		case organizeSubgraphToolName:
			if organizer == nil {
				return nil, fmt.Errorf("%s: nil organizer", organizeSubgraphToolName)
			}
			out = append(out, OrganizeSubgraphTool(organizer))
		case memoryDropFromContextToolName:
			out = append(out, DropFromContextTool(loop))
		default:
			if memory == nil {
				memory = memoryToolsByName()
			}
			if tool, ok := memory[name]; ok {
				out = append(out, tool)
				continue
			}
			if files == nil {
				files = fileToolsByName()
			}
			if tool, ok := files[name]; ok {
				out = append(out, tool)
				continue
			}
			if name == bashToolName {
				out = append(out, agenttool.Bash())
				continue
			}
			return nil, fmt.Errorf("unknown tool %q", name)
		}
	}
	return out, nil
}

func memoryToolsByName() map[string]agenttool.Tool {
	listed := agenttool.MemoryTools(nil, nil)
	out := make(map[string]agenttool.Tool, len(listed))
	for _, tool := range listed {
		out[tool.Definition().Name] = tool
	}
	return out
}

func fileToolsByName() map[string]agenttool.Tool {
	listed := agenttool.FileTools()
	out := make(map[string]agenttool.Tool, len(listed))
	for _, tool := range listed {
		out[tool.Definition().Name] = tool
	}
	return out
}

func hooksFromNames(names []string, loop *Loop) (Hooks, []agenttool.Tool, error) {
	var hooks Hooks
	var hidden []agenttool.Tool
	var hasCompact, hasInject bool
	for _, name := range names {
		switch name {
		case hookInjectSubscribedMemory:
			hooks.AssembleRequest = append(
				hooks.AssembleRequest,
				InjectSubscribedMemory(loop),
			)
			if !hasInject {
				hidden = append(hidden, newInjectSubscribedMemoryTool())
				hasInject = true
			}
		case hookCompactOnOverflow:
			hooks.AfterAssistant = append(
				hooks.AfterAssistant,
				CompactOnOverflow(loop),
			)
			if !hasCompact {
				hidden = append(hidden, newCompactMemoryTool())
				hasCompact = true
			}
		case hookCommitTailOnTurnEnd:
			hooks.CommitTurn = append(hooks.CommitTurn, CommitTailOnTurnEnd(loop))
			if !hasCompact {
				hidden = append(hidden, newCompactMemoryTool())
				hasCompact = true
			}
		case hookRemindDropContextOnPressure:
			hooks.AssembleRequest = append(
				hooks.AssembleRequest,
				RemindDropContextOnPressure(loop),
			)
		default:
			return Hooks{}, nil, fmt.Errorf("unknown hook %q", name)
		}
	}
	return hooks, hidden, nil
}

func newMemoryLoop(config Config, defaultID, prompt string, organizer *Loop) (*Loop, error) {
	if config.AgentID == "" {
		config.AgentID = defaultID
	}
	if config.SystemPrompt == "" {
		config.SystemPrompt = prompt
	}
	if organizer == nil {
		var err error
		organizer, err = NewSubgraphOrganizer(Config{
			AgentID:  config.AgentID + "-organizer",
			Provider: config.Provider,
		})
		if err != nil {
			return nil, err
		}
	}
	config.Tools = append([]agenttool.Tool{OrganizeSubgraphTool(organizer)}, config.Tools...)

	loop, err := NewLoop(config)
	if err != nil {
		return nil, err
	}
	if err := ensureHiddenMemoryTools(loop); err != nil {
		return nil, err
	}
	if err := loop.AddHooks(memoryHookSet(loop)); err != nil {
		return nil, err
	}
	return loop, nil
}

// OrganizeSubgraphTool 把向子图整理 Agent 发送 query 包装成工具。
// 执行后返回新子图，并给发起请求的 Agent 订阅该子图。
func OrganizeSubgraphTool(organizer *Loop) agenttool.Tool {
	return &organizeSubgraphTool{organizer: organizer}
}

func bindRequesters(loop *Loop, tools map[string]agenttool.Tool) {
	for _, tool := range tools {
		if binder, ok := tool.(requesterBinder); ok {
			binder.BindRequester(loop)
		}
	}
}

func (t *organizeSubgraphTool) BindRequester(loop *Loop) {
	t.requester = loop
}

func (t *organizeSubgraphTool) BindEnv(e env.Env) agenttool.Tool {
	next := *t
	next.memory = e.Memory
	return &next
}

func (*organizeSubgraphTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        organizeSubgraphToolName,
		Description: "根据查询整理出一张对应的记忆子图并返回该子图。发起请求的 Agent 会自动订阅这张新子图。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"要整理进子图的查询"}},"required":["query"],"additionalProperties":false}`),
	}
}

func (t *organizeSubgraphTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.organizer == nil {
		return agenttool.Output{}, fmt.Errorf("%s: nil organizer", organizeSubgraphToolName)
	}

	var args struct {
		Query string `json:"query"`
	}
	raw := call.Arguments
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return agenttool.Output{}, fmt.Errorf("decode arguments: %w", err)
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return agenttool.Output{}, fmt.Errorf("%s: missing query", organizeSubgraphToolName)
	}

	subgraph := ctxgraph.Subgraph{
		ID:      fmt.Sprintf("sg-q-%d", nextQuerySubgraphID.Add(1)),
		Name:    query,
		Summary: query,
		Kind:    ctxgraph.SubgraphKindTask,
	}
	if t.memory == nil {
		return agenttool.Output{}, fmt.Errorf("%s: unbound memory", organizeSubgraphToolName)
	}
	graph := t.memory.Snapshot().WithSubgraph(subgraph)
	t.memory.Commit(graph)

	if _, err := t.organizer.Ask(ctx, organizeQuery(query, subgraph.ID, t.memory.Snapshot())); err != nil {
		return agenttool.Output{}, err
	}

	if found, ok := subgraphFromGraph(t.memory.Snapshot(), subgraph.ID); ok {
		subgraph = found
	}
	if t.requester != nil {
		t.requester.subscribeSubgraph(subgraph.ID)
	}

	payload, err := json.Marshal(subgraph)
	if err != nil {
		return agenttool.Output{}, fmt.Errorf("encode subgraph: %w", err)
	}
	return agenttool.Output{Content: string(payload)}, nil
}

func organizeQuery(query, subgraphID string, graph ctxgraph.Graph) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n目标子图 ID：%s\n请把相关节点加入这个子图，不要更换 ID。查询字符串不是节点 ID。\n", query, subgraphID)
	b.WriteString("\n已有子图：\n")
	if len(graph.Subgraphs) == 0 {
		b.WriteString("（无）\n")
	}
	for _, subgraph := range graph.Subgraphs {
		fmt.Fprintf(&b, "- %s kind=%s name=%s\n", subgraph.ID, subgraph.Kind, subgraph.Name)
	}
	b.WriteString("\n查询可能命中的节点：\n")
	hits := 0
	for _, node := range graph.Nodes {
		if !nodeMatchesQuery(node, query) {
			continue
		}
		hits++
		fmt.Fprintf(&b, "- %s %s\n", node.ID, node.Statement)
	}
	if hits == 0 {
		b.WriteString("（无）\n")
	}
	return b.String()
}

func nodeMatchesQuery(node ctxgraph.Node, query string) bool {
	if query == "" {
		return false
	}
	haystack := strings.TrimSpace(node.ID + " " + node.Statement)
	if haystack == "" {
		return false
	}
	if strings.Contains(haystack, query) {
		return true
	}
	if node.ID != "" && strings.Contains(query, node.ID) {
		return true
	}
	if node.Statement != "" && strings.Contains(query, node.Statement) {
		return true
	}
	for _, token := range strings.Fields(query) {
		token = strings.Trim(token, "`\"'.,;:()[]")
		if utf8.RuneCountInString(token) < 4 {
			continue
		}
		if strings.Contains(haystack, token) {
			return true
		}
	}
	return false
}

func subgraphFromGraph(graph ctxgraph.Graph, id string) (ctxgraph.Subgraph, bool) {
	for _, subgraph := range graph.Subgraphs {
		if subgraph.ID == id {
			return subgraph, true
		}
	}
	return ctxgraph.Subgraph{}, false
}
