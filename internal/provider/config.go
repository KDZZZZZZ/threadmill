// Package provider 实现由根目录配置驱动的 LLM Provider。
package provider

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.yaml.in/yaml/v3"

	threadmill "github.com/KDZZZZZZ/threadmill"
	"github.com/KDZZZZZZ/threadmill/internal/agent"
)

const (
	// ConfigFileName 是项目根目录中的固定配置文件名。
	ConfigFileName = "threadmill.yaml"
	// ConfigDirName contains user and project-local Threadmill state.
	ConfigDirName = ".threadmill"
	// UserConfigFileName is the layered user/project configuration filename.
	UserConfigFileName  = "config.yaml"
	credentialsFileName = "credentials.yaml"
	// OpenAIResponses 标识 OpenAI-compatible Responses API。
	OpenAIResponses = "openai-responses"
)

var (
	ErrInvalidConfig      = errors.New("provider: invalid config")
	ErrCredentialNotFound = errors.New("provider: credential not found")
)

// FileConfig 是 threadmill.yaml 的顶层结构。
type FileConfig struct {
	LLM     LLMConfig             `yaml:"llm"`
	Tools   agent.FileToolCatalog `yaml:"tools"`
	Prompts agent.FilePrompts     `yaml:"prompts"`
	Agents  agent.FileAgents      `yaml:"agents"`
	Exec    ExecConfig            `yaml:"exec"`
	Memory  MemoryFileConfig      `yaml:"memory"`
}

// MemoryFileConfig 配置记忆图整理行为。
type MemoryFileConfig struct {
	Curation agent.CurationConfig `yaml:"curation"`
}

// ExecConfig 配置命令执行槽位、超时、输出上限和沙箱边界。
type ExecConfig struct {
	Slots           int    `yaml:"slots"`
	Timeout         int    `yaml:"timeout"`       // 秒；0 表示只跟 ctx
	OutputCapKB     int    `yaml:"output_cap_kb"` // 0 时调度器默认 256KB
	ContainerImage  string `yaml:"container_image"`
	ExternalSandbox bool   `yaml:"external_sandbox"`
}

// LLMConfig 配置一个 OpenAI-compatible Responses API Provider。
type LLMConfig struct {
	Provider      string `yaml:"provider"`
	BaseURL       string `yaml:"base_url"`
	Credential    string `yaml:"credential"`
	Model         string `yaml:"model"`
	ContextWindow int    `yaml:"context_window"`
}

// LoadConfig 从 root/threadmill.yaml 读取并严格校验配置。
func LoadConfig(root string) (FileConfig, error) {
	return LoadConfigFile(filepath.Join(root, ConfigFileName))
}

// LoadRuntimeConfig layers user and project overrides over the built-in config.
func LoadRuntimeConfig(root, explicitPath string) (FileConfig, error) {
	merged, err := decodeConfigMap(
		"built-in threadmill.yaml",
		strings.NewReader(threadmill.DefaultConfigYAML()),
	)
	if err != nil {
		return FileConfig{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return FileConfig{}, fmt.Errorf("find user home: %w", err)
	}
	layers := []struct {
		path     string
		required bool
	}{
		{path: filepath.Join(home, ConfigDirName, UserConfigFileName)},
		{path: filepath.Join(root, ConfigFileName)},
		{path: filepath.Join(root, ConfigDirName, UserConfigFileName)},
	}
	if explicitPath != "" {
		layers = append(layers, struct {
			path     string
			required bool
		}{path: explicitPath, required: true})
	}
	for _, layer := range layers {
		overlay, found, err := loadConfigMap(layer.path, layer.required)
		if err != nil {
			return FileConfig{}, err
		}
		if found {
			mergeConfigMap(merged, overlay)
		}
	}
	data, err := yaml.Marshal(merged)
	if err != nil {
		return FileConfig{}, fmt.Errorf("encode merged provider config: %w", err)
	}
	return decodeConfig("merged runtime configuration", bytes.NewReader(data))
}

// NeedsSetup reports whether an interactive launch has no user or project config.
func NeedsSetup(root, explicitPath string) (bool, error) {
	if explicitPath != "" {
		return false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, fmt.Errorf("find user home: %w", err)
	}
	paths := []string{
		filepath.Join(home, ConfigDirName, UserConfigFileName),
		filepath.Join(root, ConfigFileName),
		filepath.Join(root, ConfigDirName, UserConfigFileName),
	}
	for _, path := range paths {
		_, err := os.Stat(path)
		switch {
		case err == nil:
			return false, nil
		case errors.Is(err, os.ErrNotExist):
			continue
		default:
			return false, fmt.Errorf("inspect provider config %q: %w", path, err)
		}
	}
	return true, nil
}

// SaveUserSetup stores model selection separately from the named API key.
func SaveUserSetup(config LLMConfig, apiKey string) (string, error) {
	if err := config.validate(); err != nil {
		return "", err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", fmt.Errorf("%w: API key is required", ErrInvalidConfig)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home: %w", err)
	}
	dir := filepath.Join(home, ConfigDirName)
	credentialsPath := filepath.Join(dir, credentialsFileName)
	credentials := make(map[string]string)
	if _, err := os.Stat(credentialsPath); err == nil {
		credentials, err = loadCredentials(credentialsPath)
		if err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect credentials %q: %w", credentialsPath, err)
	}
	if credentials == nil {
		credentials = make(map[string]string)
	}
	credentials[config.Credential] = apiKey
	credentialsData, err := yaml.Marshal(credentials)
	if err != nil {
		return "", fmt.Errorf("encode credentials: %w", err)
	}
	configData, err := yaml.Marshal(struct {
		LLM LLMConfig `yaml:"llm"`
	}{LLM: config})
	if err != nil {
		return "", fmt.Errorf("encode user config: %w", err)
	}
	if err := writePrivateFile(credentialsPath, credentialsData); err != nil {
		return "", fmt.Errorf("write credentials %q: %w", credentialsPath, err)
	}
	configPath := filepath.Join(dir, UserConfigFileName)
	if err := writePrivateFile(configPath, configData); err != nil {
		return "", fmt.Errorf("write user config %q: %w", configPath, err)
	}
	return configPath, nil
}

// LoadConfigFile 从指定文件读取并严格校验配置。
func LoadConfigFile(path string) (FileConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("open provider config %q: %w", path, err)
	}
	defer file.Close()
	return decodeConfig(path, file)
}

func decodeConfig(path string, reader io.Reader) (FileConfig, error) {
	decoder := yaml.NewDecoder(reader)
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
	if err := agent.ValidateToolCatalog(config.Tools); err != nil {
		return FileConfig{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := config.Prompts.Validate(); err != nil {
		return FileConfig{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := config.Agents.Validate(); err != nil {
		return FileConfig{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := config.Exec.validate(); err != nil {
		return FileConfig{}, err
	}
	if err := config.Memory.Curation.Validate(); err != nil {
		return FileConfig{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return config, nil
}

func loadConfigMap(path string, required bool) (map[string]any, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open provider config %q: %w", path, err)
	}
	overlay, decodeErr := decodeConfigMap(path, file)
	closeErr := file.Close()
	if decodeErr != nil {
		return nil, false, errors.Join(decodeErr, closeErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("close provider config %q: %w", path, closeErr)
	}
	return overlay, true, nil
}

func decodeConfigMap(path string, reader io.Reader) (map[string]any, error) {
	decoder := yaml.NewDecoder(reader)
	config := make(map[string]any)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode provider config %q: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple yaml documents are not allowed")
		}
		return nil, fmt.Errorf("decode provider config %q: %w", path, err)
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode provider config %q: %w", path, err)
	}
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	var shape FileConfig
	if err := strict.Decode(&shape); err != nil {
		return nil, fmt.Errorf("decode provider config %q: %w", path, err)
	}
	return config, nil
}

func mergeConfigMap(base, overlay map[string]any) {
	for key, value := range overlay {
		baseMap, baseOK := base[key].(map[string]any)
		overlayMap, overlayOK := value.(map[string]any)
		if baseOK && overlayOK {
			mergeConfigMap(baseMap, overlayMap)
			continue
		}
		base[key] = value
	}
}

func writePrivateFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".threadmill-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		if temp != nil {
			err = errors.Join(err, temp.Close())
		}
		if !keep {
			removeErr := os.Remove(tempPath)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove temporary config: %w", removeErr))
			}
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	temp = nil
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	keep = true
	return nil
}

func (c *ExecConfig) validate() error {
	if c.Slots < 0 {
		return fmt.Errorf("%w: exec.slots must not be negative", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.ContainerImage) != c.ContainerImage {
		return fmt.Errorf("%w: exec.container_image must not have surrounding whitespace", ErrInvalidConfig)
	}
	if c.Slots == 0 {
		c.Slots = runtime.NumCPU()
	}
	return nil
}

// validate 拒绝缺失字段和不能安全构造请求地址的配置。
func (config LLMConfig) validate() error {
	if strings.TrimSpace(config.Provider) != config.Provider ||
		strings.TrimSpace(config.BaseURL) != config.BaseURL ||
		strings.TrimSpace(config.Credential) != config.Credential ||
		strings.TrimSpace(config.Model) != config.Model {
		return fmt.Errorf("%w: llm fields must not have surrounding whitespace", ErrInvalidConfig)
	}
	if config.Provider != OpenAIResponses {
		return fmt.Errorf("%w: llm.provider must be %q", ErrInvalidConfig, OpenAIResponses)
	}
	if config.Credential == "" {
		return fmt.Errorf("%w: llm.credential is required", ErrInvalidConfig)
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

func (config LLMConfig) resolveAPIKey() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: find user home: %v", ErrInvalidConfig, err)
	}
	path := filepath.Join(home, ConfigDirName, credentialsFileName)
	credentials, err := loadCredentials(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %w", ErrCredentialNotFound, err)
		}
		return "", err
	}

	apiKey := strings.TrimSpace(credentials[config.Credential])
	if apiKey == "" {
		return "", fmt.Errorf(
			"%w: %w: credential %q is empty or missing",
			ErrInvalidConfig,
			ErrCredentialNotFound,
			config.Credential,
		)
	}
	return apiKey, nil
}

func loadCredentials(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open credentials %q: %w", ErrInvalidConfig, path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: inspect credentials %q: %v", ErrInvalidConfig, path, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: credentials %q must only be accessible by its owner", ErrInvalidConfig, path)
	}

	decoder := yaml.NewDecoder(file)
	var credentials map[string]string
	if err := decoder.Decode(&credentials); err != nil {
		return nil, fmt.Errorf("%w: decode credentials %q", ErrInvalidConfig, path)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: decode credentials %q", ErrInvalidConfig, path)
	}
	return credentials, nil
}

// isLoopbackHost 只为本地 OpenAI-compatible 开发服务放行明文 HTTP。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
