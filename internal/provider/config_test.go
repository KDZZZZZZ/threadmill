package provider

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	want := FileConfig{LLM: LLMConfig{
		Provider:      "openai-responses",
		BaseURL:       "https://api.openai.com/v1",
		APIKeyEnv:     "TEST_OPENAI_API_KEY",
		Model:         "gpt-5",
		ContextWindow: 128000,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
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
