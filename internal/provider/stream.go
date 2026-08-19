package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/KDZZZZZZ/threadmill/internal/event"
)

// postStream 发送 stream=true 请求，解析 Responses SSE。
// 协议来源：https://platform.openai.com/docs/guides/streaming-responses
func (transport transport) postStream(ctx context.Context, payload any, sink func(string)) (createResponseResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return createResponseResponse{}, fmt.Errorf("encode provider request: %w", err)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		transport.endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return createResponseResponse{}, fmt.Errorf("create provider request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+transport.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")

	response, err := transport.client.Do(request)
	if err != nil {
		return createResponseResponse{}, fmt.Errorf("send provider request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
		if err != nil {
			return createResponseResponse{}, fmt.Errorf("read provider response: %w", err)
		}
		return createResponseResponse{}, decodeHTTPError(response.Status, responseBody)
	}
	return readResponseStream(response.Body, sink)
}

func readResponseStream(r io.Reader, sink func(string)) (createResponseResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxResponseBody)

	var eventName, data string
	var completed *createResponseResponse
	var streamed strings.Builder
	total := 0
	dispatch := func() error {
		if data == "" {
			eventName = ""
			return nil
		}
		total += len(data)
		if total > maxResponseBody {
			return errors.New("provider response exceeds 16 MiB")
		}
		var payload struct {
			Type     string                  `json:"type"`
			Delta    string                  `json:"delta"`
			Response *createResponseResponse `json:"response"`
			createResponseResponse
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			eventName = ""
			data = ""
			return fmt.Errorf("decode responses stream event: %w", err)
		}
		typ := eventName
		if typ == "" {
			typ = payload.Type
		}
		switch typ {
		case "response.output_text.delta":
			if payload.Delta != "" {
				streamed.WriteString(payload.Delta)
				if sink != nil {
					sink(payload.Delta)
				}
			}
		case "response.completed", "response.done":
			resp := payload.Response
			if resp == nil {
				copied := payload.createResponseResponse
				resp = &copied
			}
			if resp.Status == "" {
				resp.Status = "completed"
			}
			if err := fillOutputFromDeltas(resp, streamed.String()); err != nil {
				return err
			}
			completed = resp
		case "response.incomplete":
			if payload.Response != nil {
				completed = payload.Response
			}
		case "response.failed", "error":
			return fmt.Errorf("responses stream %s", typ)
		}
		eventName = ""
		data = ""
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return createResponseResponse{}, err
			}
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if chunk == "[DONE]" {
				continue
			}
			if data != "" {
				data += "\n"
			}
			data += chunk
		}
	}
	if err := dispatch(); err != nil {
		return createResponseResponse{}, err
	}
	if err := scanner.Err(); err != nil {
		return createResponseResponse{}, fmt.Errorf("read responses stream: %w", err)
	}
	if completed == nil {
		return createResponseResponse{}, errors.New("responses stream ended without response.completed")
	}
	return *completed, nil
}

func fillOutputFromDeltas(resp *createResponseResponse, deltas string) error {
	if resp == nil || len(resp.Output) > 0 || deltas == "" {
		return nil
	}
	item, err := json.Marshal(responseOutput{
		Type: "message",
		Content: []responseContent{{
			Type: "output_text",
			Text: deltas,
		}},
	})
	if err != nil {
		return fmt.Errorf("encode streamed responses output: %w", err)
	}
	resp.Output = []json.RawMessage{item}
	return nil
}

// WithDeltaSink 把流式文本回调挂到 ctx 上。
func WithDeltaSink(ctx context.Context, sink func(string)) context.Context {
	return event.WithDeltaSink(ctx, sink)
}
