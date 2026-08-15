// Package provider 实现由根目录配置驱动的 LLM Provider。
package provider

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
)

const (
	// ConfigFileName 是项目根目录中的固定配置文件名。
	ConfigFileName = "threadmill.yaml"
	// OpenAIResponses 标识 OpenAI-compatible Responses API。
	OpenAIResponses = "openai-responses"
)

var ErrInvalidConfig = errors.New("provider: invalid config")

// FileConfig 是 threadmill.yaml 的顶层结构。
type FileConfig struct {
	LLM    LLMConfig       `yaml:"llm"`
	Agents agent.FileAgents `yaml:"agents"`
}

// LLMConfig 配置一个 OpenAI-compatible Responses API Provider。
type LLMConfig struct {
	Provider      string `yaml:"provider"`
	BaseURL       string `yaml:"base_url"`
	APIKeyEnv     string `yaml:"api_key_env"`
	Model         string `yaml:"model"`
	ContextWindow int    `yaml:"context_window"`
}

// LoadConfig 从 root/threadmill.yaml 读取并严格校验配置。
func LoadConfig(root string) (FileConfig, error) {
	path := filepath.Join(root, ConfigFileName)
	file, err := os.Open(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("open provider config %q: %w", path, err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	// YAML Decoder 官方文档：https://pkg.go.dev/go.yaml.in/yaml/v3#Decoder.KnownFields
	decoder.KnownFields(true)

	var config FileConfig
	if err := decoder.Decode(&config); err != nil {
		return FileConfig{}, fmt.Errorf("decode provider config %q: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple yaml documents are not allowed")
		}
		return FileConfig{}, fmt.Errorf("decode provider config %q: %w", path, err)
	}
	if err := config.LLM.validate(); err != nil {
		return FileConfig{}, err
	}
	if err := config.Agents.Validate(); err != nil {
		return FileConfig{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return config, nil
}

// validate 拒绝缺失字段和不能安全构造请求地址的配置。
func (config LLMConfig) validate() error {
	if strings.TrimSpace(config.Provider) != config.Provider ||
		strings.TrimSpace(config.BaseURL) != config.BaseURL ||
		strings.TrimSpace(config.APIKeyEnv) != config.APIKeyEnv ||
		strings.TrimSpace(config.Model) != config.Model {
		return fmt.Errorf("%w: llm fields must not have surrounding whitespace", ErrInvalidConfig)
	}
	if config.Provider != OpenAIResponses {
		return fmt.Errorf("%w: llm.provider must be %q", ErrInvalidConfig, OpenAIResponses)
	}
	if strings.TrimSpace(config.APIKeyEnv) == "" {
		return fmt.Errorf("%w: llm.api_key_env is required", ErrInvalidConfig)
	}
	if strings.TrimSpace(config.Model) == "" {
		return fmt.Errorf("%w: llm.model is required", ErrInvalidConfig)
	}
	if config.ContextWindow < 0 {
		return fmt.Errorf("%w: llm.context_window must not be negative", ErrInvalidConfig)
	}

	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return fmt.Errorf("%w: llm.base_url must be an absolute http(s) URL", ErrInvalidConfig)
	}
	if baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return fmt.Errorf("%w: llm.base_url must not contain a query or fragment", ErrInvalidConfig)
	}
	if baseURL.Scheme == "http" && !isLoopbackHost(baseURL.Hostname()) {
		return fmt.Errorf("%w: llm.base_url must use https outside loopback", ErrInvalidConfig)
	}
	return nil
}

// isLoopbackHost 只为本地 OpenAI-compatible 开发服务放行明文 HTTP。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
