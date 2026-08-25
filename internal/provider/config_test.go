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
		{"manager", config.Agents.Manager.SystemPrompt, "PASS 缺少 Task Info 逐项映射或必要门禁"},
		{"planner", config.Agents.Planner.SystemPrompt, "持久测试"},
		{"executor", config.Agents.Executor.SystemPrompt, "持久测试"},
		{"verifier", config.Agents.Verifier.SystemPrompt, "任务新增验收"},
	}
	for _, check := range checks {
		if !strings.Contains(check.prompt, check.want) {
			t.Errorf("%s prompt missing %q", check.role, check.want)
		}
	}
}

func TestRepositoryOrganizerPromptPreservesCumulativeContract(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		config.Agents.SubgraphOrganizer.SystemPrompt,
		"新的局部缺陷不能挤掉原始累计契约",
	) {
		t.Fatal("subgraph organizer prompt does not preserve the cumulative contract")
	}
}

func TestRepositoryPlannerPromptRechecksUpstreamClaims(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		config.Agents.Planner.SystemPrompt,
		"上游报告只是线索，必须用当前工作区核对",
	) {
		t.Fatal("planner prompt does not recheck upstream claims")
	}
}

func TestRepositoryPlannerPromptTreatsRepairRootsIncrementally(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"最近失败证据和原始累计契约",
		"不重复已经失败的同型方案",
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
		"期望值来自 Task Info、既有文档或既有测试，不从实现输出反推",
		"逐字 API/字段/字符串/退出码/默认值",
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
		"Task Info 承诺的公共入口",
		"优先选择能击穿充分性漏洞的反例输入",
		"测试数量",
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
		"不同于提交测试输入",
		"Task Info 承诺的公共入口",
		"自写测试是线索，不是自动可信的 oracle",
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
		"正负边界",
		"合理反例",
		"内部实现形状当作目的成立的代理条件",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt accepts proxy evidence for negative behavior %q", want)
		}
	}
}

func TestRepositoryVerifierPromptCoversStatefulBoundaries(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"跨调用状态",
		"遗漏边界",
		"相邻回归风险",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not cover stateful boundaries %q", want)
		}
	}
}

func TestRepositoryVerifierPromptCoversComposedContracts(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"逐字 API/字段/字符串/退出码/默认值",
		"所有 Ci 都成立但 G 仍未实现是否可能",
		"验收条件的合取",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not cover composed contracts %q", want)
		}
	}
}

func TestRepositoryRepairPromptsRejectCollapsedCumulativeEvidence(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		role   string
		prompt string
		wants  []string
	}{
		{
			role:   "manager",
			prompt: config.Agents.Manager.SystemPrompt,
			wants:  []string{"原始累计契约", "Task Info 逐项映射"},
		},
		{
			role:   "verifier",
			prompt: config.Agents.Verifier.SystemPrompt,
			wants:  []string{"完整累计契约", "将每个 Ci 映射"},
		},
	}
	for _, check := range checks {
		for _, want := range check.wants {
			if !strings.Contains(check.prompt, want) {
				t.Fatalf("%s prompt accepts collapsed cumulative evidence %q", check.role, want)
			}
		}
	}
}

func TestRepositoryVerifierPromptCoversTemporalState(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"跨调用状态",
		"合理反例",
		"公共入口",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not cover temporal state %q", want)
		}
	}
}

func TestRepositoryRepairPromptsPreserveAndExerciseNamedPublicMethods(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"逐字接口/输出",
		"原始累计契约",
	} {
		if !strings.Contains(config.Agents.Manager.SystemPrompt, want) {
			t.Fatalf("manager prompt loses named public methods in repair roots %q", want)
		}
	}
	for _, want := range []string{
		"逐字 API/字段/字符串/退出码/默认值",
		"Task Info 承诺的公共入口",
		"直接证据",
	} {
		if !strings.Contains(config.Agents.Verifier.SystemPrompt, want) {
			t.Fatalf("verifier prompt does not exercise named public methods %q", want)
		}
	}
}

func TestRepositoryManagerAuditsVerifierEvidence(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"verifier 报告是待审计证据",
		"PASS 缺少 Task Info 逐项映射或必要门禁",
	} {
		if !strings.Contains(config.Agents.Manager.SystemPrompt, want) {
			t.Fatalf("manager prompt does not audit verifier evidence %q", want)
		}
	}
}

func TestRepositoryDefaultPromptRecoversFromToolFailures(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config.Prompts.Default, "工具失败时先根据错误恢复") {
		t.Error("default prompt treats a recoverable mechanical mismatch as a blocker")
	}
}

func TestRepositoryManagerOnlyExtendsForActionableFailures(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"只有可由工作区改动消除的缺陷才继续扩图",
		"workflow done 不等于验收通过",
	} {
		if !strings.Contains(config.Agents.Manager.SystemPrompt, want) {
			t.Fatalf("manager prompt extends for non-actionable failures %q", want)
		}
	}
}

func TestRepositoryManagerPromptDoesNotPrequeueDuplicateRepairs(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"以全局图去重跨 task 的重复工作",
		"同型缺陷连续出现时",
	} {
		if !strings.Contains(config.Agents.Manager.SystemPrompt, want) {
			t.Fatalf("manager prompt does not avoid duplicate repair roots %q", want)
		}
	}
}

func TestRepositoryPlannerPromptBatchesReadyWork(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"同一 frontier 中全部写入不冲突的单元放进同一 concurrency group",
		"共享前置只做一次，完成后立即 fan-out",
	} {
		if !strings.Contains(config.Agents.Planner.SystemPrompt, want) {
			t.Errorf("planner prompt does not batch ready work %q", want)
		}
	}
}

func TestRepositoryOnlyExecutorRequestsHelp(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []struct {
		name  string
		tools []string
		want  bool
	}{
		{name: "planner", tools: config.Agents.Planner.Tools},
		{name: "executor", tools: config.Agents.Executor.Tools, want: true},
		{name: "verifier", tools: config.Agents.Verifier.Tools},
	} {
		if got := slices.Contains(role.tools, "coordination_requestHelp"); got != role.want {
			t.Errorf("%s coordination_requestHelp = %v, want %v", role.name, got, role.want)
		}
	}
}

func TestRepositoryRolePromptsRequireAlternativeEvidencePaths(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		role   string
		prompt string
		want   string
	}{
		{"planner", config.Agents.Planner.SystemPrompt, "无法执行时列替代路径和未覆盖面"},
		{"verifier", config.Agents.Verifier.SystemPrompt, "合理替代路径均已尝试"},
	}
	for _, check := range checks {
		if !strings.Contains(check.prompt, check.want) {
			t.Errorf("%s prompt does not exercise fallback branches", check.role)
		}
	}
}

func TestRepositoryRolePromptsPreserveExactPublicContracts(t *testing.T) {
	config, err := LoadConfigFile(filepath.Join("..", "..", ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		role   string
		prompt string
		wants  []string
	}{
		{"planner", config.Agents.Planner.SystemPrompt, []string{"逐字保留 Task Info 的硬要求", "构建/编译"}},
		{"executor", config.Agents.Executor.SystemPrompt, []string{"Task Info", "构建/编译"}},
		{"verifier", config.Agents.Verifier.SystemPrompt, []string{"逐字 API/字段/字符串/退出码/默认值", "Task Info 承诺的公共入口"}},
	}
	for _, check := range checks {
		for _, want := range check.wants {
			if !strings.Contains(check.prompt, want) {
				t.Errorf("%s prompt does not preserve exact public examples %q", check.role, want)
			}
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
	if !strings.Contains(got.Prompts.Default, "每个来源必须 apply 或 discard，结束角色前 finish") {
		t.Fatal("workspace prompts.default missing join disposition contract")
	}
	if got.Agents.Manager.SystemPrompt == "" {
		t.Fatal("workspace manager system_prompt is empty")
	}
	for _, want := range []string{
		"核心职责是编排和维护全局协调图",
		"manager 决定 root 与 helper 的放置、依赖、准入、去重和增量改图",
		"planner 的拆分提案是 manager 编排全局图的输入，不是对全局图的直接决定",
		"普通用户消息与 `[拆分请求]` 是两种不同输入",
		"普通用户消息不得触发 coordination_provideHelp",
		"manager 永远不调用、也不声称调用 coordination_requestHelp",
		"工具成功返回前，不得声称 task、helper 或图变更已经创建",
		"寒暄和直接问答不要主动介绍内部角色、协调图或 help 协议",
		"workflow done 不等于验收通过",
	} {
		if !strings.Contains(got.Agents.Manager.SystemPrompt, want) {
			t.Errorf("workspace manager system_prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"width_class", "target_width", "cluster_active_width"} {
		if strings.Contains(got.Agents.Manager.SystemPrompt, unwanted) {
			t.Errorf("workspace manager system_prompt contains planner detail %q", unwanted)
		}
	}
	for _, want := range []string{
		"验收对象是 Task Info 所表达的预期目的",
		"尽可能成为 G 的充要条件",
		"对每个拟阻断结论做高置信复核",
		"未证实的可能性、风格偏好和一般质量建议只能列为非阻断观察",
	} {
		if !strings.Contains(got.Agents.Verifier.SystemPrompt, want) {
			t.Errorf("workspace verifier system_prompt missing %q", want)
		}
	}
	for _, role := range []struct {
		name  string
		tools []string
	}{
		{"planner", got.Agents.Planner.Tools},
		{"executor", got.Agents.Executor.Tools},
		{"verifier", got.Agents.Verifier.Tools},
	} {
		if !slices.Contains(role.tools, "join") {
			t.Errorf("workspace %s tools = %v, want join", role.name, role.tools)
		}
	}
	if slices.Contains(got.Agents.Manager.Tools, "join") {
		t.Fatalf("manager tools include role-local %q", "join")
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
