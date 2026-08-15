package provider

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadConfigReadsRootYAML(t *testing.T) {
	root := t.TempDir()
	content := []byte(`llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  api_key_env: TEST_OPENAI_API_KEY
  model: gpt-5
`)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	want := FileConfig{LLM: LLMConfig{
		Provider:  "openai-responses",
		BaseURL:   "https://api.openai.com/v1",
		APIKeyEnv: "TEST_OPENAI_API_KEY",
		Model:     "gpt-5",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadConfig() = %#v, want %#v", got, want)
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
