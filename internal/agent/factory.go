package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const subgraphOrganizerID = "subgraph-organizer"
const organizeSubgraphToolName = "organize_subgraph"

const (
	memoryNeighborsToolName       = "memory_neighbors"
	memorySubgraphsOfToolName     = "memory_subgraphs_of"
	memorySourcesOfToolName       = "memory_sources_of"
	memoryNodesInToolName         = "memory_nodes_in"
	memoryAddToSubgraphToolName   = "memory_add_to_subgraph"
	memoryDropFromContextToolName = "memory_drop_from_context"
	memoryExpandToolName          = "memory_expand"
	memoryCollapseToolName        = "memory_collapse"

	fileReadToolName  = "read"
	fileWriteToolName = "write"
	fileEditToolName  = "edit"
	fileLsToolName    = "ls"
	fileGrepToolName  = "grep"
	fileFindToolName  = "find"
	bashToolName      = "bash"

	coordOrchestrateToolName = "coordination_orchestrate"
	coordPublishTaskToolName = "coordination_publishTask"
	coordRequestHelpToolName = "coordination_requestHelp"
	coordJoinToolName        = "join"

	hookInjectSubscribedMemory      = "inject_subscribed_memory"
	hookCompactOnOverflow           = "compact_on_overflow"
	hookCommitTailOnTurnEnd         = "commit_tail_on_turn_end"
	hookRemindDropContextOnPressure = "remind_drop_context_on_pressure"
)

const (
	plannerID  = "planner"
	executorID = "executor"
	verifierID = "verifier"
	managerID  = "manager"
)

// querySubgraphPrefix 是 organize 查询子图的 ID 前缀；序号从当前图里已有的最大值往后取，
// 这样重启后不会撞上持久化图里的同名子图、把订阅目标悄悄换成别的内容。
const querySubgraphPrefix = "sg-q-"

var knownFileTools = map[string]struct{}{
	organizeSubgraphToolName:      {},
	memoryNeighborsToolName:       {},
	memorySubgraphsOfToolName:     {},
	memorySourcesOfToolName:       {},
	memoryNodesInToolName:         {},
	memoryAddToSubgraphToolName:   {},
	memoryDropFromContextToolName: {},
	memoryExpandToolName:          {},
	memoryCollapseToolName:        {},
	memorySubscribeToolName:       {},
	agenttool.MemoryApplyName:     {},
	fileReadToolName:              {},
	fileWriteToolName:             {},
	fileEditToolName:              {},
	fileLsToolName:                {},
	fileGrepToolName:              {},
	fileFindToolName:              {},
	bashToolName:                  {},
	coordOrchestrateToolName:      {},
	coordPublishTaskToolName:      {},
	coordRequestHelpToolName:      {},
	coordJoinToolName:             {},
}

// organizerOnlyTools 只允许 subgraph_organizer 装配：记忆图的批量改写权集中在整理 Agent。
var organizerOnlyTools = map[string]struct{}{
	agenttool.MemoryApplyName: {},
	memorySubscribeToolName:   {},
}

var knownFileHooks = map[string]struct{}{
	hookInjectSubscribedMemory:      {},
	hookCompactOnOverflow:           {},
	hookCommitTailOnTurnEnd:         {},
	hookRemindDropContextOnPressure: {},
}

var managerOnlyTools = map[string]struct{}{
	coordOrchestrateToolName: {},
	coordPublishTaskToolName: {},
}

var taskRoleOnlyTools = map[string]struct{}{
	coordJoinToolName: {},
}

// FileAgent 是 threadmill.yaml 里单个 Agent 的配置。
type FileAgent struct {
	ID           string   `yaml:"id"`
	MaxSteps     int      `yaml:"max_steps"`
	SystemPrompt string   `yaml:"system_prompt"`
	Tools        []string `yaml:"tools"`
	Hooks        []string `yaml:"hooks"`
}

// FileTool 是 threadmill.yaml 里一条工具介绍，发给模型的 Description。
type FileTool struct {
	Description string `yaml:"description"`
}

// FileToolCatalog 是顶层 tools 名到介绍的映射。
type FileToolCatalog map[string]FileTool

// FilePrompts 是 threadmill.yaml 里发给模型的提示词。空字段不覆盖。
type FilePrompts struct {
	Default             string `yaml:"default"`
	Compact             string `yaml:"compact"`
	CompactJSONReminder string `yaml:"compact_json_reminder"`
	DropContextPressure string `yaml:"drop_context_pressure"`
	OrganizeQuery       string `yaml:"organize_query"`
	SessionResetHandoff string `yaml:"session_reset_handoff"`
}

// FileOverlay 把 yaml 的 tools/prompts 盖到装配出的 Agent 上，并可注入运行时工具和事件总线。
type FileOverlay struct {
	Tools      FileToolCatalog
	Prompts    FilePrompts
	NamedTools map[string]agenttool.Tool
	Events     *event.Bus
	Curation   CurationConfig
	// NoSessionReset 关掉整理 Agent 的窗口压力会话重置。生产默认开启；
	// 这个开关只给评测做同 workload 的 A/B 对照用。
	NoSessionReset bool
	// SubscriptionAttribution 让订阅注入块按来源子图分组并标注归属，
	// 使订阅者能看见边界、自行降权不相干节点。默认关闭，等 A/B 数据再定默认值。
	SubscriptionAttribution bool
}

// FileAgents 是 threadmill.yaml 里内置 Agent 的配置。
type FileAgents struct {
	Manager           FileAgent `yaml:"manager"`
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
		{"manager", agents.Manager},
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
	for _, tool := range role.Tools {
		if _, ok := taskRoleOnlyTools[tool]; ok && name != "planner" && name != "executor" && name != "verifier" {
			return fmt.Errorf("agents.%s.tools: %q is task-role-only", name, tool)
		}
		if name == "manager" {
			continue
		}
		if _, ok := managerOnlyTools[tool]; ok {
			return fmt.Errorf("agents.%s.tools: %q is manager-only", name, tool)
		}
		if name != "subgraph_organizer" {
			if _, ok := organizerOnlyTools[tool]; ok {
				return fmt.Errorf("agents.%s.tools: %q is organizer-only", name, tool)
			}
		}
	}
	return nil
}

func (p FilePrompts) Validate() error {
	for _, item := range []struct {
		name  string
		value string
	}{
		{"prompts.default", p.Default},
		{"prompts.compact", p.Compact},
		{"prompts.compact_json_reminder", p.CompactJSONReminder},
		{"prompts.drop_context_pressure", p.DropContextPressure},
		{"prompts.organize_query", p.OrganizeQuery},
		{"prompts.session_reset_handoff", p.SessionResetHandoff},
	} {
		if strings.TrimSpace(item.value) != item.value {
			return fmt.Errorf("%s must not have surrounding whitespace", item.name)
		}
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

	loop, err := NewLoop(config)
	if err != nil {
		return nil, err
	}

	tools, definitions, err := prepareTools(append(
		append(agenttool.MemoryTools(nil, nil), MemorySubscribeTool()),
		extra...,
	))
	if err != nil {
		return nil, err
	}
	loop.tools = tools
	loop.definitions = definitions
	loop.sessionReset = true
	return loop, nil
}

// NewPlanner 创建规划 Agent，挂上记忆 hook，并注册向子图整理 Agent 发 query 的工具。
func NewPlanner(config Config) (*Loop, error) {
	return newMemoryLoop(config, plannerID, nil)
}

// NewExecutor 创建执行 Agent，挂上记忆 hook，并注册向子图整理 Agent 发 query 的工具。
func NewExecutor(config Config) (*Loop, error) {
	return newMemoryLoop(config, executorID, nil)
}

// NewVerifier 创建核验 Agent，挂上记忆 hook，并注册向子图整理 Agent 发 query 的工具。
func NewVerifier(config Config) (*Loop, error) {
	return newMemoryLoop(config, verifierID, nil)
}

// NewTeam 按 yaml 配置装配四个 Agent，上下文窗口来自模型，规划/执行/核验共用同一个整理 Agent。
func NewTeam(
	provider Provider,
	contextWindow int,
	agents FileAgents,
	extra []agenttool.Tool,
	overlay ...FileOverlay,
) (*Team, error) {
	if err := agents.Validate(); err != nil {
		return nil, err
	}
	o := firstOverlay(overlay)

	organizerCfg := agents.SubgraphOrganizer.loopConfig(provider, nil)
	organizerCfg.ContextWindow = contextWindow
	organizer, err := newFileLoop(
		organizerCfg,
		agents.SubgraphOrganizer,
		subgraphOrganizerID,
		nil,
		o,
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
		organizer,
		o,
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
		organizer,
		o,
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
		organizer,
		o,
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

// NewManager 装配对接用户的经理 Agent；协调图工具通过 overlay.NamedTools 注入。
func NewManager(
	provider Provider,
	contextWindow int,
	agents FileAgents,
	extra []agenttool.Tool,
	overlay ...FileOverlay,
) (*Loop, error) {
	if err := agents.Validate(); err != nil {
		return nil, err
	}
	o := firstOverlay(overlay)
	organizerCfg := agents.SubgraphOrganizer.loopConfig(provider, nil)
	organizerCfg.ContextWindow = contextWindow
	organizer, err := newFileLoop(
		organizerCfg,
		agents.SubgraphOrganizer,
		subgraphOrganizerID,
		nil,
		o,
	)
	if err != nil {
		return nil, err
	}
	managerCfg := agents.Manager.loopConfig(provider, extra)
	managerCfg.ContextWindow = contextWindow
	return newFileLoop(
		managerCfg,
		agents.Manager,
		managerID,
		organizer,
		o,
	)
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

// BindEvents 把同一条事件总线接到 team 里所有 Loop。
func (t *Team) BindEvents(bus *event.Bus) {
	if t == nil {
		return
	}
	t.Planner.BindEvents(bus)
	t.Executor.BindEvents(bus)
	t.Verifier.BindEvents(bus)
	t.Organizer.BindEvents(bus)
}

func withFileID(config Config, defaultID string) Config {
	if config.AgentID == "" {
		config.AgentID = defaultID
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
	loop.memory = e.Memory
	// 换环境必须丢掉记忆投影 memo：memo 只按 revision + 订阅列表判等，而 Fork 出的子图
	// 与父图 revision 相同（Graph.Clone 保留 Revision），订阅列表也跨 Bind 保留，
	// 不清会把上一个环境的记忆文本泄漏给新环境。
	loop.memoryBlockRev = 0
	loop.memoryBlockSubs = nil
	loop.memoryStableBlockText = ""
	loop.memoryBlockText = ""
	if organizer := organizerFromTool(loop.tools[organizeSubgraphToolName]); organizer != nil {
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
	organizer *Loop,
	overlay FileOverlay,
) (*Loop, error) {
	config = withFileID(config, defaultID)
	if config.SystemPrompt == "" {
		config.SystemPrompt = overlay.Prompts.Default
	}
	if config.Events == nil {
		config.Events = overlay.Events
	}
	loop, err := NewLoop(config)
	if err != nil {
		return nil, err
	}
	if err := installNamedPlugins(loop, file.Tools, file.Hooks, organizer, overlay.NamedTools); err != nil {
		return nil, fmt.Errorf("install %s plugins: %w", loop.agentID, err)
	}
	if err := applyFileOverlay(loop, overlay); err != nil {
		return nil, err
	}
	if defaultID == subgraphOrganizerID {
		loop.sessionReset = !overlay.NoSessionReset
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

// ValidateToolCatalog 拒绝未知工具名，以及缺介绍或介绍带首尾空白。
func ValidateToolCatalog(catalog FileToolCatalog) error {
	for name, spec := range catalog {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("tools must not contain an empty or padded name")
		}
		if !knownCatalogTool(name) {
			return fmt.Errorf("tools: unknown %q", name)
		}
		if strings.TrimSpace(spec.Description) == "" {
			return fmt.Errorf("tools.%s.description is required", name)
		}
		if strings.TrimSpace(spec.Description) != spec.Description {
			return fmt.Errorf("tools.%s.description must not have surrounding whitespace", name)
		}
	}
	return nil
}

func knownCatalogTool(name string) bool {
	if _, ok := knownFileTools[name]; ok {
		return true
	}
	return name == compactMemoryToolName || name == injectSubscribedMemoryToolName
}

func installNamedPlugins(
	loop *Loop,
	toolNames []string,
	hookNames []string,
	organizer *Loop,
	named map[string]agenttool.Tool,
) error {
	if len(toolNames) > 0 {
		listed, err := toolsFromNames(toolNames, loop, organizer, named)
		if err != nil {
			return err
		}
		if err := registerTools(loop, listed); err != nil {
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
	named map[string]agenttool.Tool,
) ([]agenttool.Tool, error) {
	var memory map[string]agenttool.Tool
	var files map[string]agenttool.Tool
	out := make([]agenttool.Tool, 0, len(names))
	for _, name := range names {
		if tool, ok := named[name]; ok && tool != nil {
			out = append(out, tool)
			continue
		}
		switch name {
		case organizeSubgraphToolName:
			if organizer == nil {
				return nil, fmt.Errorf("%s: nil organizer", organizeSubgraphToolName)
			}
			out = append(out, OrganizeSubgraphTool(organizer))
		case memoryDropFromContextToolName:
			out = append(out, DropFromContextTool(loop))
		case memoryExpandToolName:
			out = append(out, MemoryExpandTool(loop))
		case memoryCollapseToolName:
			out = append(out, MemoryCollapseTool(loop))
		case memorySubscribeToolName:
			out = append(out, MemorySubscribeTool())
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

func newMemoryLoop(config Config, defaultID string, organizer *Loop) (*Loop, error) {
	config = withFileID(config, defaultID)
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
// 执行后返回新子图，并给发起请求的 Agent 订阅该子图；整理期间 organizer 通过
// memory_subscribe 表达的订阅增删也在这里统一应用。
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
		Name: organizeSubgraphToolName,
		Description: "按 query 召回记忆，结果作为新子图返回并自动订阅；整理 Agent 也可能据 exclude 取消你的动态订阅。" +
			"三个正当触发：Task Info 含“记忆依赖”声明时开工第一个回合调用一次，query 逐字携带声明内容；" +
			"核对过 Task Info、已注入记忆和当前工作区后，仍缺一个会改变下一步行动的历史事实；" +
			"注入记忆里有大片与当前职责无关、或适用范围确已失效的内容，用 exclude 声明范围。" +
			"每次调用是一次完整的整理 Loop，成本高：query 一次写全全部必要条件；返回 0 个节点说明记忆图没有，改去工作区核对，不换措辞重查；" +
			"无声明且无决定级缺口时不调用。exclude 的退订是持久的，只写确已失效的范围。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"要整理进子图的查询：你需要哪些记忆"},"exclude":{"type":"string","description":"可选的负向过滤：哪些记忆你不需要，或哪些已订阅的内容对当前职责已经失效"}},"required":["query"],"additionalProperties":false}`),
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
		Query   string `json:"query"`
		Exclude string `json:"exclude"`
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
	exclude := strings.TrimSpace(args.Exclude)

	if t.memory == nil {
		return agenttool.Output{}, fmt.Errorf("%s: unbound memory", organizeSubgraphToolName)
	}
	before := t.memory.Snapshot()
	subgraph := ctxgraph.Subgraph{
		ID:      nextQuerySubgraphID(before),
		Name:    query,
		Summary: query,
		Kind:    ctxgraph.SubgraphKindTask,
	}
	graph := before.WithSubgraph(subgraph)
	if err := t.memory.Commit(graph); err != nil {
		return agenttool.Output{}, fmt.Errorf("commit organized subgraph: %w", err)
	}

	// sink 只挂在这一条 Ask 的 ctx 上：organizer 的 memory_subscribe 在别处调用会直接报错。
	sink := &subscriptionSink{}
	if err := organizeMemory(
		withSubscriptionSink(ctx, sink),
		t.organizer,
		t.memory,
		organizeSubgraphToolName,
		query,
		exclude,
		subgraph.ID,
	); err != nil {
		return agenttool.Output{}, err
	}

	snapshot := t.memory.Snapshot()
	if found, ok := subgraphFromGraph(snapshot, subgraph.ID); ok {
		subgraph = found
	}
	// 先应用 organizer 的增删，再补上目标子图的自动订阅：本次查询的结果必然可见。
	subscriptions := t.applySubscriptions(sink, snapshot, subgraph.ID)
	if t.requester != nil {
		t.requester.subscribeSubgraph(subgraph.ID)
	}

	return marshalToolJSON(organizeSubgraphResult{
		Subgraph:      subgraph,
		Subscriptions: subscriptions,
	})
}

// organizeSubgraphResult 是 organize_subgraph 的返回：整理出的子图，加上本次查询对请求方
// 动态订阅的实际改动。逐项报告而不是整体失败——个别 ID 无效不该让整次整理白做。
type organizeSubgraphResult struct {
	Subgraph      ctxgraph.Subgraph   `json:"subgraph"`
	Subscriptions subscriptionOutcome `json:"subscriptions"`
}

type subscriptionOutcome struct {
	Subscribed   []string           `json:"subscribed"`
	Unsubscribed []string           `json:"unsubscribed"`
	Skipped      []subscriptionSkip `json:"skipped,omitempty"`
}

type subscriptionSkip struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// applySubscriptions 把 organizer 记录的订阅增删落到请求方身上。
// subscribe 需要子图在当前快照里真实存在；unsubscribe 只过滤动态订阅列表，
// 任务启动包（stable）与运行时固定订阅（fixed）在结构上不可取消。
func (t *organizeSubgraphTool) applySubscriptions(
	sink *subscriptionSink,
	graph ctxgraph.Graph,
	targetID string,
) subscriptionOutcome {
	subscribe, unsubscribe := sink.pending()
	outcome := subscriptionOutcome{Subscribed: []string{}, Unsubscribed: []string{}}
	if len(subscribe) == 0 && len(unsubscribe) == 0 {
		return outcome
	}
	if t.requester == nil {
		for _, id := range append(append([]string(nil), subscribe...), unsubscribe...) {
			outcome.Skipped = append(outcome.Skipped, subscriptionSkip{
				ID:     id,
				Reason: "no requester is bound to this organize_subgraph tool",
			})
		}
		return outcome
	}
	for _, id := range subscribe {
		if _, ok := subgraphFromGraph(graph, id); !ok {
			outcome.Skipped = append(outcome.Skipped, subscriptionSkip{
				ID:     id,
				Reason: "subgraph does not exist in the current memory graph",
			})
			continue
		}
		t.requester.subscribeSubgraph(id)
		outcome.Subscribed = append(outcome.Subscribed, id)
	}
	for _, id := range unsubscribe {
		if id == targetID {
			outcome.Skipped = append(outcome.Skipped, subscriptionSkip{
				ID:     id,
				Reason: "this is the subgraph organized for the current query and stays subscribed",
			})
			continue
		}
		if !t.requester.unsubscribeSubgraph(id) {
			outcome.Skipped = append(outcome.Skipped, subscriptionSkip{
				ID:     id,
				Reason: "not a dynamic subscription; the task package and runtime-fixed subscriptions cannot be dropped",
			})
			continue
		}
		outcome.Unsubscribed = append(outcome.Unsubscribed, id)
	}
	return outcome
}

// nextQuerySubgraphID 在当前图已有的 sg-q-N 之后取号，避免重启后与持久化子图重名。
func nextQuerySubgraphID(graph ctxgraph.Graph) string {
	highest := uint64(0)
	for _, subgraph := range graph.Subgraphs {
		rest, ok := strings.CutPrefix(subgraph.ID, querySubgraphPrefix)
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(rest, 10, 64)
		if err != nil {
			continue
		}
		if n > highest {
			highest = n
		}
	}
	return fmt.Sprintf("%s%d", querySubgraphPrefix, highest+1)
}

func organizeMemory(
	ctx context.Context,
	organizer *Loop,
	memory env.MemoryView,
	operation, query, exclude, subgraphID string,
) error {
	before := memory.Snapshot()
	started := time.Now()
	organizer.noteSessionSubgraph(subgraphID)
	organizer.publish(ctx, event.MemoryStart(organizer.agentID, operation, subgraphID))
	_, err := organizer.Ask(ctx, organizeQuery(
		query,
		exclude,
		subgraphID,
		before,
		organizer.organizeQueryText(),
	))
	selected := len(memory.Snapshot().NodesInSubgraphs([]string{subgraphID}))
	organizer.publish(ctx, event.MemoryOrganized(
		organizer.agentID,
		operation,
		subgraphID,
		started,
		countQueryMatches(before, query),
		selected,
		err,
	))
	return err
}

func organizeQuery(query, exclude, subgraphID string, graph ctxgraph.Graph, instruction string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", query)
	if exclude != "" {
		fmt.Fprintf(&b, "\n请求方声明不需要的记忆：%s\n", exclude)
	}
	fmt.Fprintf(&b, "\n目标子图 ID：%s\n%s\n", subgraphID, instruction)
	b.WriteString("\n已有子图：\n")
	if len(graph.Subgraphs) == 0 {
		b.WriteString("（无）\n")
	}
	for _, subgraph := range graph.Subgraphs {
		fmt.Fprintf(&b, "- %s kind=%s name=%s\n", subgraph.ID, subgraph.Kind, subgraph.Name)
		writeSubgraphDescription(&b, subgraph, "  ")
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

// countQueryMatches 数查询消息里真实列出的候选节点数。
// 报告里的“候选”必须和模型实际看到的候选列表一致；用全图节点数会把召回率读成几乎为零。
func countQueryMatches(graph ctxgraph.Graph, query string) int {
	matches := 0
	for _, node := range graph.Nodes {
		if nodeMatchesQuery(node, query) {
			matches++
		}
	}
	return matches
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
