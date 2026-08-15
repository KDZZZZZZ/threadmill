package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	maxResponseBody       = 16 << 20
	defaultRequestTimeout = 2 * time.Minute
)

// transport 保存 OpenAI-compatible Provider 共用的 HTTP 配置。
type transport struct {
	client   *http.Client
	endpoint string
	apiKey   string
	model    string
}

// newTransport 校验协议类型并构造对应 API 端点。
func newTransport(
	config LLMConfig,
	expectedProvider string,
	endpointPath string,
	client *http.Client,
) (transport, error) {
	if err := config.validate(); err != nil {
		return transport{}, err
	}
	if config.Provider != expectedProvider {
		return transport{}, fmt.Errorf(
			"%w: llm.provider must be %q",
			ErrInvalidConfig,
			expectedProvider,
		)
	}

	apiKey, exists := os.LookupEnv(config.APIKeyEnv)
	if !exists || strings.TrimSpace(apiKey) == "" {
		return transport{}, fmt.Errorf(
			"%w: environment variable %q is empty",
			ErrInvalidConfig,
			config.APIKeyEnv,
		)
	}
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}

	baseURL, _ := url.Parse(config.BaseURL)
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + endpointPath
	baseURL.RawPath = ""
	return transport{
		client:   client,
		endpoint: baseURL.String(),
		apiKey:   strings.TrimSpace(apiKey),
		model:    config.Model,
	}, nil
}

// post 发送 JSON 请求并解码有大小上限的 JSON 响应。
func (transport transport) post(ctx context.Context, payload any, output any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode provider request: %w", err)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		transport.endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("create provider request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+transport.apiKey)
	request.Header.Set("Content-Type", "application/json")

	response, err := transport.client.Do(request)
	if err != nil {
		return fmt.Errorf("send provider request: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	if err != nil {
		return fmt.Errorf("read provider response: %w", err)
	}
	if len(responseBody) > maxResponseBody {
		return errors.New("provider response exceeds 16 MiB")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return decodeHTTPError(response.Status, responseBody)
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return fmt.Errorf("decode provider response: %w", err)
	}
	return nil
}

// decodeHTTPError 提取兼容 OpenAI 错误信封的消息，但不暴露请求密钥。
func decodeHTTPError(status string, body []byte) error {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		return fmt.Errorf("provider API %s: %s", status, envelope.Error.Message)
	}
	return fmt.Errorf("provider API %s", status)
}
