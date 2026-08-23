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

func testLLMConfig(t *testing.T, baseURL string) LLMConfig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".threadmill")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.yaml"), []byte("test: test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return LLMConfig{
		Provider: OpenAIResponses, BaseURL: baseURL, Credential: "test", Model: "gpt-5",
	}
}

func TestLoadConfigReadsRootYAML(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  credential: test
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
			Credential:    "test",
			Model:         "gpt-5",
			ContextWindow: 128000,
		},
		Exec: ExecConfig{Slots: runtime.NumCPU()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
	}
}

func TestLoadConfigFileReadsNamedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  credential: test
  model: gpt-5
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.LLM.Model != "gpt-5" {
		t.Fatalf("model = %q, want gpt-5", got.LLM.Model)
	}
}

func TestLoadRuntimeConfigUsesBuiltInDefaultsWithoutFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := LoadRuntimeConfig(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.LLM.Provider != OpenAIResponses {
		t.Fatalf("provider = %q, want %q", got.LLM.Provider, OpenAIResponses)
	}
	if got.Prompts.Default == "" {
		t.Fatal("built-in default prompt is empty")
	}
	if got.Agents.Manager.ID != "manager" {
		t.Fatalf("manager id = %q, want manager", got.Agents.Manager.ID)
	}
}

func TestLoadRuntimeConfigLayersUserThenProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userDir := filepath.Join(home, ".threadmill")
	if err := os.Mkdir(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "config.yaml"), []byte(`llm:
  credential: personal
  model: user-model
`), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	projectDir := filepath.Join(root, ".threadmill")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config.yaml"), []byte(`llm:
  model: project-model
  context_window: 64000
`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadRuntimeConfig(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.LLM.Credential != "personal" {
		t.Fatalf("credential = %q, want personal", got.LLM.Credential)
	}
	if got.LLM.Model != "project-model" {
		t.Fatalf("model = %q, want project-model", got.LLM.Model)
	}
	if got.LLM.ContextWindow != 64000 {
		t.Fatalf("context window = %d, want 64000", got.LLM.ContextWindow)
	}
	if got.Prompts.Default == "" {
		t.Fatal("built-in default prompt was lost")
	}
}

func TestLoadRuntimeConfigReportsUnknownFieldSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userDir := filepath.Join(home, ConfigDirName)
	if err := os.Mkdir(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(userDir, UserConfigFileName)
	if err := os.WriteFile(configPath, []byte(`llm:
  moddle: typo
`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadRuntimeConfig(t.TempDir(), "")
	if err == nil {
		t.Fatal("LoadRuntimeConfig() error = nil, want unknown field error")
	}
	if !strings.Contains(err.Error(), configPath) {
		t.Fatalf("LoadRuntimeConfig() error = %v, want source path %q", err, configPath)
	}
}

func TestLoadRuntimeConfigAppliesExplicitOverrideLast(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userDir := filepath.Join(home, ConfigDirName)
	if err := os.Mkdir(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(userDir, UserConfigFileName),
		[]byte("llm:\n  model: user-model\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, ConfigFileName),
		[]byte("llm:\n  model: legacy-project-model\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(root, ConfigDirName)
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(projectDir, UserConfigFileName),
		[]byte("llm:\n  model: project-model\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	explicitPath := filepath.Join(t.TempDir(), "override.yaml")
	if err := os.WriteFile(
		explicitPath,
		[]byte("llm:\n  model: explicit-model\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	got, err := LoadRuntimeConfig(root, explicitPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.LLM.Model != "explicit-model" {
		t.Fatalf("model = %q, want explicit-model", got.LLM.Model)
	}
}

func TestSaveUserSetupWritesPrivateConfigAndCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	userDir := filepath.Join(home, ConfigDirName)
	if err := os.Mkdir(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(userDir, "credentials.yaml"),
		[]byte("existing: sk-existing\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	needsSetup, err := NeedsSetup(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !needsSetup {
		t.Fatal("NeedsSetup() = false before user or project configuration exists")
	}

	configPath, err := SaveUserSetup(LLMConfig{
		Provider:      OpenAIResponses,
		BaseURL:       "https://api.openai.com/v1",
		Credential:    "personal",
		Model:         "gpt-test",
		ContextWindow: 200000,
	}, "sk-super-secret")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(home, ConfigDirName, UserConfigFileName)
	if configPath != wantPath {
		t.Fatalf("config path = %q, want %q", configPath, wantPath)
	}
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configData), "sk-super-secret") {
		t.Fatal("user config contains API key")
	}
	credentialsPath := filepath.Join(home, ConfigDirName, "credentials.yaml")
	credentialsData, err := os.ReadFile(credentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(credentialsData), "personal: sk-super-secret") {
		t.Fatalf("credentials = %q, want saved personal credential", credentialsData)
	}
	if !strings.Contains(string(credentialsData), "existing: sk-existing") {
		t.Fatalf("credentials = %q, want existing credential preserved", credentialsData)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{configPath, credentialsPath} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode for %q = %o, want 600", path, info.Mode().Perm())
			}
		}
	}

	needsSetup, err = NeedsSetup(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if needsSetup {
		t.Fatal("NeedsSetup() = true after user setup was saved")
	}
}

func TestMissingCredentialCanTriggerFirstTimeSetup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_, err := newTransport(LLMConfig{
		Provider:   OpenAIResponses,
		BaseURL:    "https://api.openai.com/v1",
		Credential: "missing",
		Model:      "gpt-test",
	}, OpenAIResponses, "/responses", nil)
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("newTransport() error = %v, want ErrCredentialNotFound", err)
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("newTransport() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadConfigRejectsProjectAPIKey(t *testing.T) {
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

	if _, err := LoadConfig(root); err == nil {
		t.Fatal("LoadConfig() error = nil, want unknown field api_key")
	}
}

func TestLoadConfigRejectsMissingCredential(t *testing.T) {
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

func TestNewTransportUsesUserCredentialFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".threadmill")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.yaml"), []byte("opencode: sk-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := newTransport(LLMConfig{
		Provider:   OpenAIResponses,
		BaseURL:    "https://api.openai.com/v1",
		Credential: "opencode",
		Model:      "gpt-5",
	}, OpenAIResponses, "/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.apiKey != "sk-from-file" {
		t.Fatalf("apiKey = %q", got.apiKey)
	}
}

func TestNewTransportHasNoRequestTimeoutByDefault(t *testing.T) {
	got, err := newTransport(
		testLLMConfig(t, "https://api.openai.com/v1"),
		OpenAIResponses,
		"/responses",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.client.Timeout != 0 {
		t.Fatalf("client timeout = %s, want no timeout", got.client.Timeout)
	}
}

func TestNewTransportRejectsInsecureCredentialFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".threadmill")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.yaml")
	if err := os.WriteFile(path, []byte("opencode: sk-must-not-leak\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := newTransport(LLMConfig{
		Provider:   OpenAIResponses,
		BaseURL:    "https://api.openai.com/v1",
		Credential: "opencode",
		Model:      "gpt-5",
	}, OpenAIResponses, "/responses", nil)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("newTransport() error = %v, want ErrInvalidConfig", err)
	}
	if strings.Contains(err.Error(), "sk-must-not-leak") {
		t.Fatalf("newTransport() error leaks credential: %v", err)
	}
}

func TestCredentialDecodeErrorDoesNotExposeFileContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".threadmill")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.yaml"), []byte("sk-must-not-leak\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := newTransport(LLMConfig{
		Provider:   OpenAIResponses,
		BaseURL:    "https://api.openai.com/v1",
		Credential: "opencode",
		Model:      "gpt-5",
	}, OpenAIResponses, "/responses", nil)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("newTransport() error = %v, want ErrInvalidConfig", err)
	}
	if strings.Contains(err.Error(), "sk-must-not-leak") {
		t.Fatalf("newTransport() error leaks credential: %v", err)
	}
}

func TestLoadConfigReadsAgents(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  credential: test
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
			Credential:    "test",
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

func TestRepositoryRolePromptsRequirePersistentTestEvidence(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		role   string
		prompt string
		want   string
	}{
		{"manager", config.Agents.Manager.SystemPrompt, "提交中的测试文件"},
		{"planner", config.Agents.Planner.SystemPrompt, "持久回归测试矩阵"},
		{"executor", config.Agents.Executor.SystemPrompt, "提交到工作区的回归测试"},
		{"verifier", config.Agents.Verifier.SystemPrompt, "临时 CLI/脚本"},
	}
	for _, check := range checks {
		if !strings.Contains(check.prompt, check.want) {
			t.Errorf("%s prompt missing %q", check.role, check.want)
		}
	}
}

func TestRepositoryOrganizerPromptRejectsStaleWorkflowState(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		config.Agents.SubgraphOrganizer.SystemPrompt,
		"已被当前 Task Info 或协调图取代的临时状态",
	) {
		t.Fatal("subgraph organizer prompt does not reject stale workflow state")
	}
}

func TestRepositoryPlannerPromptRejectsMissingPathsAsEvidence(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		config.Agents.Planner.SystemPrompt,
		"工具已确认不存在的路径",
	) {
		t.Fatal("planner prompt does not reject missing paths as evidence")
	}
}

func TestRepositoryPlannerPromptTreatsRepairRootsIncrementally(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"继承前序工作区的修复 task",
		"不要重复已经有稳定证据的全仓调查",
	} {
		if !strings.Contains(config.Agents.Planner.SystemPrompt, want) {
			t.Fatalf("planner prompt does not keep repair planning incremental %q", want)
		}
	}
}

func TestRepositoryVerifierPromptDerivesExpectedOutputs(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"测试中的 expected 不是独立证据",
		"返回类型、返回形状",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not independently derive expected output contract %q", want)
		}
	}
}

func TestRepositoryVerifierPromptExercisesSemanticFailurePaths(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"实现路径确实被执行",
		"失败后的状态",
		"测试名称、测试数量",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not exercise semantic failure paths %q", want)
		}
	}
}

func TestRepositoryVerifierPromptUsesFreshIndependentProbes(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"不修改测试文件",
		"不同输入",
		"唯一行为证据",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not require a fresh independent probe %q", want)
		}
	}
}

func TestRepositoryVerifierPromptObservesNegativeBehavior(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"抑制、删除、不产生或覆盖",
		"缺失、数量减少或替换后的最终结果",
		"内部 class、flag",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt accepts proxy evidence for negative behavior %q", want)
		}
	}
}

func TestRepositoryVerifierPromptProbesRecursiveIsolation(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"clone、copy 或 transfer 的隔离",
		"深度大于一层",
		"从源和目标两侧交替修改",
		"closure capture 或嵌套 callable 图",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not probe recursive isolation %q", want)
		}
	}
}

func TestRepositoryVerifierPromptProbesComposedProtocolFields(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"可在同一输入中共存",
		"至少构造两两组合",
		"不同公共编码形态",
		"分别通过不能证明组合语义",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not probe composed protocol fields %q", want)
		}
	}
}

func TestRepositoryRepairPromptsRejectCollapsedCumulativeEvidence(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for role, prompt := range map[string]string{
		"manager":  config.Agents.Manager.SystemPrompt,
		"verifier": config.Agents.Verifier.SystemPrompt,
	} {
		for _, want := range []string{
			"每个句子或分号子句",
			"测试总数",
			"相关路径已覆盖",
		} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("%s prompt accepts collapsed cumulative evidence %q", role, want)
			}
		}
	}
}

func TestRepositoryVerifierPromptProbesColdAndWarmObservationOrder(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tracker、subscription、cache 或 lazy initialization",
		"事件发生后才首次创建或观察",
		"预热后再触发事件",
		"消费后的重复观察",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not probe cold and warm observation order %q", want)
		}
	}
}

func TestRepositoryRepairPromptsPreserveAndExerciseNamedPublicMethods(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"公开协议的方法签名",
		"精确输出字符串",
		"所属模块或类型",
		"逐字保留",
	} {
		if !strings.Contains(config.Agents.Manager.SystemPrompt, want) {
			t.Fatalf("manager prompt loses named public methods in repair roots %q", want)
		}
	}
	for _, want := range []string{
		"每个明确命名的公开方法",
		"所属模块、类型或 receiver",
		"至少调用一次",
		"独立进程或实例",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not exercise named public methods %q", want)
		}
	}
}

func TestRepositoryManagerRejectsSelfAuthoredTestOnlyEvidence(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"唯一行为证据",
		"不同输入的独立临时 probe",
	} {
		if !strings.Contains(config.Agents.Manager.SystemPrompt, want) {
			t.Fatalf("manager prompt accepts self-authored-test-only evidence %q", want)
		}
	}
}

func TestRepositoryRolePromptsDoNotBlockOnMissingBaselineAlias(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		role   string
		prompt string
	}{
		{"manager", config.Agents.Manager.SystemPrompt},
		{"planner", config.Agents.Planner.SystemPrompt},
		{"executor", config.Agents.Executor.SystemPrompt},
		{"verifier", config.Agents.Verifier.SystemPrompt},
	}
	for _, check := range checks {
		if !strings.Contains(check.prompt, "基线分支名不存在") {
			t.Errorf("%s prompt treats a missing baseline alias as a blocker", check.role)
		}
	}
}

func TestRepositoryManagerPromptDoesNotRequireEmptyRepairCommit(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"后继持久工作区已经满足报告中的修复条件",
		"不要要求空提交",
	} {
		if !strings.Contains(config.Agents.Manager.SystemPrompt, want) {
			t.Fatalf("manager prompt does not accept a no-op repair %q", want)
		}
	}
}

func TestRepositoryManagerPromptDoesNotPrequeueDuplicateRepairs(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"一次任务报告只追加一层最小修复 root",
		"不要预排依赖该修复结果的同质后继 root",
	} {
		if !strings.Contains(config.Agents.Manager.SystemPrompt, want) {
			t.Fatalf("manager prompt does not avoid duplicate repair roots %q", want)
		}
	}
}

func TestRepositoryWorkspaceRolePromptsBatchIndependentTools(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		role   string
		prompt string
	}{
		{"planner", config.Agents.Planner.SystemPrompt},
		{"executor", config.Agents.Executor.SystemPrompt},
		{"verifier", config.Agents.Verifier.SystemPrompt},
	}
	for _, check := range checks {
		if !strings.Contains(check.prompt, "互不依赖的工具调用") {
			t.Errorf("%s prompt does not batch independent tools", check.role)
		}
	}
}

func TestRepositoryWorkspaceRolePromptsExerciseFallbackBranches(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		role   string
		prompt string
	}{
		{"planner", config.Agents.Planner.SystemPrompt},
		{"executor", config.Agents.Executor.SystemPrompt},
		{"verifier", config.Agents.Verifier.SystemPrompt},
	}
	for _, check := range checks {
		if !strings.Contains(check.prompt, "优选资源不存在") {
			t.Errorf("%s prompt does not exercise fallback branches", check.role)
		}
	}
}

func TestRepositoryWorkspaceRolePromptsCompileExactPublicExamples(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		role   string
		prompt string
	}{
		{"planner", config.Agents.Planner.SystemPrompt},
		{"executor", config.Agents.Executor.SystemPrompt},
		{"verifier", config.Agents.Verifier.SystemPrompt},
	}
	for _, check := range checks {
		if !strings.Contains(check.prompt, "公开调用示例") {
			t.Errorf("%s prompt does not compile exact public examples", check.role)
		}
	}
}

func TestLoadConfigRejectsNegativeAgentMaxSteps(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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

func TestLoadConfigAcceptsExecContainerImage(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  credential: test
  model: gpt-5
exec:
  container_image: golang:1.26.5-alpine
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec.ContainerImage != "golang:1.26.5-alpine" {
		t.Fatalf("Exec.ContainerImage = %q", got.Exec.ContainerImage)
	}
}

func TestLoadConfigAcceptsExternalSandbox(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  credential: test
  model: gpt-5
exec:
  external_sandbox: true
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exec.ExternalSandbox {
		t.Fatal("Exec.ExternalSandbox = false, want true")
	}
}

func TestLoadConfigRejectsPaddedExecContainerImage(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  credential: test
  model: gpt-5
exec:
  container_image: " golang:1.26.5-alpine "
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
  credential: test
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
  credential: test
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
  credential: test
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
  credential: test
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
		"每个角色先 fork 自己的工作区，再处理 join，最后 Ask",
		"join 时子 task 记忆合入 task 共享记忆",
		".threadmill/runtime/joins/manifest.json",
		"目标角色才用已有的文件和命令工具筛选改动、解决冲突",
		"join 到 planner：子 task 文件只进入一次性规划工作区",
		"join 到 executor：子 task 文件进入 task 持久文件环境",
		"join 到 verifier：子 task 文件只进入一次性核验工作区",
		"新 root 会从前一个 root 的 task 持久环境（记忆和文件）fork",
		"不要回退或覆盖已完成 task 的实现",
		"修复焦点可以增量收窄，但验收范围不能随之收窄",
		"已完成的辅助分支由系统自动保留",
	} {
		if !strings.Contains(got.Agents.Manager.SystemPrompt, want) {
			t.Errorf("workspace manager system_prompt missing %q", want)
		}
	}
	if !strings.Contains(got.Agents.Verifier.SystemPrompt, "发现实现缺陷只写入核验报告") {
		t.Fatal("workspace verifier system_prompt missing report-only defect guidance")
	}
	if !strings.Contains(got.Agents.Verifier.SystemPrompt, "按完整累计验收清单复验") {
		t.Fatal("workspace verifier system_prompt missing cumulative repair verification")
	}
	if !strings.Contains(got.Agents.Verifier.SystemPrompt, "枚举的保护区域、模式或变体") {
		t.Fatal("workspace verifier system_prompt missing enumerated behavior evidence")
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
