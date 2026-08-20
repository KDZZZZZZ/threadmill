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
	"strings"
	"time"
)

const (
	maxResponseBody       = 16 << 20
	defaultRequestTimeout = 2 * time.Minute
	maxRequestRetries     = 5
	defaultRetryInterval  = time.Second
)

// transport 保存 OpenAI-compatible Provider 共用的 HTTP 配置。
type transport struct {
	client        *http.Client
	endpoint      string
	apiKey        string
	model         string
	retryInterval time.Duration
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

	apiKey, err := config.resolveAPIKey()
	if err != nil {
		return transport{}, err
	}
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}

	baseURL, _ := url.Parse(config.BaseURL)
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + endpointPath
	baseURL.RawPath = ""
	return transport{
		client:        client,
		endpoint:      baseURL.String(),
		apiKey:        strings.TrimSpace(apiKey),
		model:         config.Model,
		retryInterval: defaultRetryInterval,
	}, nil
}

// post 发送 JSON 请求并解码有大小上限的 JSON 响应。
func (transport transport) post(ctx context.Context, payload any, output any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode provider request: %w", err)
	}

	response, err := transport.do(ctx, body, "")
	if err != nil {
		return err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	if err != nil {
		return fmt.Errorf("read provider response: %w", err)
	}
	if len(responseBody) > maxResponseBody {
		return errors.New("provider response exceeds 16 MiB")
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return fmt.Errorf("decode provider response: %w", err)
	}
	return nil
}

// do 最多重试五次尚未开始交付响应体的瞬时请求失败。
func (transport transport) do(ctx context.Context, body []byte, accept string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			transport.endpoint,
			bytes.NewReader(body),
		)
		if err != nil {
			return nil, fmt.Errorf("create provider request: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+transport.apiKey)
		request.Header.Set("Content-Type", "application/json")
		if accept != "" {
			request.Header.Set("Accept", accept)
		}

		response, err := transport.client.Do(request)
		if err == nil && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			return response, nil
		}
		if err != nil {
			err = fmt.Errorf("send provider request: %w", err)
			if ctx.Err() != nil || attempt >= maxRequestRetries {
				return nil, err
			}
		} else {
			responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
			response.Body.Close()
			if readErr != nil {
				return nil, fmt.Errorf("read provider response: %w", readErr)
			}
			if len(responseBody) > maxResponseBody {
				return nil, errors.New("provider response exceeds 16 MiB")
			}
			err = decodeHTTPError(response.Status, responseBody)
			if !retryableStatus(response.StatusCode) || attempt >= maxRequestRetries {
				return nil, err
			}
		}
		if err := waitRetry(ctx, transport.retryInterval); err != nil {
			return nil, err
		}
	}
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusConflict ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}

func waitRetry(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait to retry provider request: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
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
