package provider

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
)

func TestLoadConfigReadsRootYAML(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: TEST_OPENAI_API_KEY
  model: gpt-5
  context_window: 128000
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	want := FileConfig{
		LLM: LLMConfig{
			Provider:      "openai-responses",
			BaseURL:       "https://api.openai.com/v1",
			APIKeyEnv:     "TEST_OPENAI_API_KEY",
			Model:         "gpt-5",
			ContextWindow: 128000,
		},
		Exec: ExecConfig{Slots: runtime.NumCPU()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}
}

func TestLoadConfigReadsAPIKey(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key: sk-test-literal
  model: gpt-5
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.LLM.APIKey != "sk-test-literal" {
		t.Fatalf("APIKey = %q, want sk-test-literal", got.LLM.APIKey)
	}
}

func TestLoadConfigRejectsMissingAPIKey(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  model: gpt-5
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(root)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestNewTransportUsesYAMLAPIKeyWithoutEnv(t *testing.T) {
	got, err := newTransport(LLMConfig{
		Provider: OpenAIResponses,
		BaseURL:  "https://api.openai.com/v1",
		APIKey:   "sk-from-yaml",
		Model:    "gpt-5",
	}, OpenAIResponses, "/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.apiKey != "sk-from-yaml" {
		t.Fatalf("apiKey = %q", got.apiKey)
	}
}

func TestLoadConfigReadsAgents(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: TEST_OPENAI_API_KEY
  model: gpt-5
  context_window: 128000
agents:
  manager:
    id: manager
    max_steps: 48
    system_prompt: |-
      manager prompt
    tools:
      - coordination_replacePending
    hooks:
      - inject_subscribed_memory
  planner:
    id: planner
    max_steps: 32
    system_prompt: |-
      plan prompt
    tools:
      - organize_subgraph
    hooks:
      - inject_subscribed_memory
      - compact_on_overflow
      - commit_tail_on_turn_end
  executor:
    id: executor
    max_steps: 64
    system_prompt: |-
      execute prompt
    tools:
      - organize_subgraph
    hooks:
      - inject_subscribed_memory
      - compact_on_overflow
      - commit_tail_on_turn_end
  verifier:
    id: verifier
    max_steps: 16
    system_prompt: |-
      verify prompt
    tools:
      - organize_subgraph
    hooks:
      - inject_subscribed_memory
      - compact_on_overflow
      - commit_tail_on_turn_end
  subgraph_organizer:
    id: subgraph-organizer
    max_steps: 24
    system_prompt: |-
      organize prompt
    tools:
      - memory_neighbors
      - memory_subgraphs_of
      - memory_sources_of
      - memory_nodes_in
      - memory_add_to_subgraph
      - memory_drop_from_context
    hooks:
      - remind_drop_context_on_pressure
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	want := FileConfig{
		LLM: LLMConfig{
			Provider:      "openai-responses",
			BaseURL:       "https://api.openai.com/v1",
			APIKeyEnv:     "TEST_OPENAI_API_KEY",
			Model:         "gpt-5",
			ContextWindow: 128000,
		},
		Agents: agent.FileAgents{
			Manager: agent.FileAgent{
				ID:           "manager",
				MaxSteps:     48,
				SystemPrompt: "manager prompt",
				Tools:        []string{"coordination_replacePending"},
				Hooks:        []string{"inject_subscribed_memory"},
			},
			Planner: agent.FileAgent{
				ID:           "planner",
				MaxSteps:     32,
				SystemPrompt: "plan prompt",
				Tools:        []string{"organize_subgraph"},
				Hooks: []string{
					"inject_subscribed_memory",
					"compact_on_overflow",
					"commit_tail_on_turn_end",
				},
			},
			Executor: agent.FileAgent{
				ID:           "executor",
				MaxSteps:     64,
				SystemPrompt: "execute prompt",
				Tools:        []string{"organize_subgraph"},
				Hooks: []string{
					"inject_subscribed_memory",
					"compact_on_overflow",
					"commit_tail_on_turn_end",
				},
			},
			Verifier: agent.FileAgent{
				ID:           "verifier",
				MaxSteps:     16,
				SystemPrompt: "verify prompt",
				Tools:        []string{"organize_subgraph"},
				Hooks: []string{
					"inject_subscribed_memory",
					"compact_on_overflow",
					"commit_tail_on_turn_end",
				},
			},
			SubgraphOrganizer: agent.FileAgent{
				ID:           "subgraph-organizer",
				MaxSteps:     24,
				SystemPrompt: "organize prompt",
				Tools: []string{
					"memory_neighbors",
					"memory_subgraphs_of",
					"memory_sources_of",
					"memory_nodes_in",
					"memory_add_to_subgraph",
					"memory_drop_from_context",
				},
				Hooks: []string{"remind_drop_context_on_pressure"},
			},
		},
		Exec: ExecConfig{Slots: runtime.NumCPU()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}
}

func TestLoadConfigRejectsNegativeAgentMaxSteps(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
agents:
  planner:
    max_steps: -1
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigAcceptsFileTools(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: TEST_OPENAI_API_KEY
  model: gpt-5
agents:
  planner:
    tools:
      - read
      - ls
      - grep
      - find
  executor:
    tools:
      - read
      - write
      - edit
  verifier:
    tools:
      - read
      - ls
      - grep
      - find
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Agents.Planner.Tools, []string{"read", "ls", "grep", "find"}) {
		t.Fatalf("planner tools = %v", got.Agents.Planner.Tools)
	}
	if !reflect.DeepEqual(got.Agents.Executor.Tools, []string{"read", "write", "edit"}) {
		t.Fatalf("executor tools = %v", got.Agents.Executor.Tools)
	}
	if !reflect.DeepEqual(got.Agents.Verifier.Tools, []string{"read", "ls", "grep", "find"}) {
		t.Fatalf("verifier tools = %v", got.Agents.Verifier.Tools)
	}
}

func TestLoadConfigReadsToolCatalog(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: TEST_OPENAI_API_KEY
  model: gpt-5
tools:
  read:
    description: 读取工作区文本文件。
  compact_memory:
    description: 把旧对话整理进记忆图。
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	want := agent.FileToolCatalog{
		"read":           {Description: "读取工作区文本文件。"},
		"compact_memory": {Description: "把旧对话整理进记忆图。"},
	}
	if !reflect.DeepEqual(got.Tools, want) {
		t.Fatalf("tools = %#v, want %#v", got.Tools, want)
	}
}

func TestLoadConfigReadsPrompts(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: TEST_OPENAI_API_KEY
  model: gpt-5
prompts:
  default: generic agent
  compact: compact memory
  compact_json_reminder: json only
  drop_context_pressure: drop nodes
  organize_query: add nodes to subgraph
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	want := agent.FilePrompts{
		Default:             "generic agent",
		Compact:             "compact memory",
		CompactJSONReminder: "json only",
		DropContextPressure: "drop nodes",
		OrganizeQuery:       "add nodes to subgraph",
	}
	if !reflect.DeepEqual(got.Prompts, want) {
		t.Fatalf("prompts = %#v, want %#v", got.Prompts, want)
	}
}

func TestLoadConfigRejectsUnknownCatalogTool(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
tools:
  not_a_tool:
    description: unknown
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigRejectsUnknownAgentTool(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
agents:
  planner:
    tools:
      - not_a_tool
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigRejectsPlannerGraphTool(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
agents:
  planner:
    tools:
      - coordination_replacePending
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigRejectsUnknownAgentHook(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
agents:
  planner:
    hooks:
      - not_a_hook
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigRejectsDuplicateAgentTool(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
agents:
  planner:
    tools:
      - organize_subgraph
      - organize_subgraph
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigRejectsAgentContextWindow(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
  context_window: 128000
agents:
  planner:
    context_window: 8000
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); err == nil {
		t.Fatal("LoadConfig() error = nil, want unknown field context_window")
	}
}

func TestLoadConfigRejectsRemoteHTTP(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: http://example.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(root)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigDefaultsExecSlotsToNumCPU(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec.Slots != runtime.NumCPU() {
		t.Fatalf("Exec.Slots = %d, want runtime.NumCPU()=%d", got.Exec.Slots, runtime.NumCPU())
	}
}

func TestLoadConfigRejectsNegativeExecSlots(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
exec:
  slots: -1
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigAcceptsBashTool(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: TEST_OPENAI_API_KEY
  model: gpt-5
agents:
  executor:
    tools:
      - bash
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Agents.Executor.Tools, []string{"bash"}) {
		t.Fatalf("executor tools = %v, want [bash]", got.Agents.Executor.Tools)
	}
}

func TestLoadConfigRejectsNegativeContextWindow(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
  context_window: -1
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigRejectsCatalogToolMissingDescription(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
tools:
  read: {}
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigRejectsPaddedPrompt(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: gpt-5
prompts:
  compact: " padded "
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigReadsWorkspaceFile(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate config test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "../.."))

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tools["read"].Description == "" {
		t.Fatal("workspace tools.read.description is empty")
	}
	if got.Prompts.Compact == "" || got.Prompts.OrganizeQuery == "" {
		t.Fatalf("workspace prompts missing: %#v", got.Prompts)
	}
	if got.Agents.Planner.SystemPrompt == "" {
		t.Fatal("workspace planner system_prompt is empty")
	}
	for _, role := range []struct {
		name  string
		tools []string
	}{
		{"planner", got.Agents.Planner.Tools},
		{"verifier", got.Agents.Verifier.Tools},
	} {
		for _, name := range []string{"write", "edit", "bash"} {
			if !slices.Contains(role.tools, name) {
				t.Errorf("workspace %s tools = %v, want %q", role.name, role.tools, name)
			}
		}
	}
	if got.Prompts.Default == "" {
		t.Fatal("workspace prompts.default is empty")
	}
	if got.Agents.Manager.SystemPrompt == "" {
		t.Fatal("workspace manager system_prompt is empty")
	}
	for _, want := range []string{
		"join 到 planner：子任务输出、记忆和实现进入一次性规划环境",
		"join 到 executor：子任务输出、记忆和实现先进入 task 持久环境",
		"join 到 verifier：子任务输出、记忆和实现先进入 task 持久环境，再 fork 一次性核验环境",
	} {
		if !strings.Contains(got.Agents.Manager.SystemPrompt, want) {
			t.Errorf("workspace manager system_prompt missing %q", want)
		}
	}
	if got.Tools["coordination_replacePending"].Description == "" {
		t.Fatal("workspace tools.coordination_replacePending.description is empty")
	}
	for _, name := range got.Agents.Planner.Tools {
		if name == "coordination_replacePending" {
			t.Fatalf("planner tools include manager-only %q", name)
		}
	}
	for _, name := range got.Agents.Manager.Tools {
		switch name {
		case "read", "write", "edit", "ls", "grep", "find", "bash":
			t.Fatalf("manager tools include file/exec %q", name)
		}
	}
	if got.Tools["read"].Description != "读取工作区文本文件。offset 是从 1 起的行号，limit 是最大行数。约 2000 行或 50KB 截断；截断时给出下一个 offset。" {
		t.Fatalf("tools.read.description = %q", got.Tools["read"].Description)
	}
}
