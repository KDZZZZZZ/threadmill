package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
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

	replayable := event.ReplayableDeltas(ctx)
	activitySink := event.DeltaActivitySink(ctx)
	retries := 0
	for {
		response, err := transport.do(ctx, body, "text/event-stream", &retries)
		if err != nil {
			return createResponseResponse{}, err
		}
		delivered := false
		var buffered []string
		result, readErr := readResponseStream(response.Body, func(delta string) {
			if replayable {
				buffered = append(buffered, delta)
				return
			}
			delivered = sink != nil
			if sink != nil {
				sink(delta)
			}
		}, activitySink)
		closeErr := response.Body.Close()
		if readErr == nil && closeErr == nil {
			for _, delta := range buffered {
				if sink != nil {
					sink(delta)
				}
			}
			return result, nil
		}
		if closeErr != nil {
			closeErr = fmt.Errorf("close responses stream: %w", closeErr)
			readErr = errors.Join(readErr, closeErr)
		}
		retryable := strings.HasPrefix(readErr.Error(), "read responses stream:") ||
			strings.Contains(readErr.Error(), "ended without response.completed") ||
			retryableResponseStreamError(readErr)
		if delivered || ctx.Err() != nil || retries >= maxRequestRetries || !retryable {
			return result, readErr
		}
		retries++
		notifyRetry(ctx, retryReasonForStreamError(readErr))
		if err := waitRetry(ctx, transport.retryInterval); err != nil {
			return createResponseResponse{}, err
		}
	}
}

func readResponseStream(r io.Reader, sink func(string), activity func(bool)) (createResponseResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxResponseBody)

	var eventName, data string
	var completed *createResponseResponse
	completedItems := make(map[int]json.RawMessage)
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
			Type        string                  `json:"type"`
			Code        string                  `json:"code"`
			Message     string                  `json:"message"`
			Delta       string                  `json:"delta"`
			OutputIndex *int                    `json:"output_index"`
			Item        json.RawMessage         `json:"item"`
			Response    *createResponseResponse `json:"response"`
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
		if activity != nil {
			activity(typ == "response.output_text.delta" && payload.Delta != "")
		}
		switch typ {
		case "response.output_text.delta":
			if payload.Delta != "" {
				streamed.WriteString(payload.Delta)
				if sink != nil {
					sink(payload.Delta)
				}
			}
		case "response.output_item.done":
			if payload.OutputIndex != nil && *payload.OutputIndex >= 0 && len(payload.Item) > 0 {
				completedItems[*payload.OutputIndex] = payload.Item
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
			mergeCompletedOutputItems(resp, completedItems)
			if err := fillOutputFromDeltas(resp, streamed.String()); err != nil {
				return err
			}
			completed = resp
		case "response.incomplete":
			if payload.Response != nil {
				completed = payload.Response
			}
		case "response.failed", "error":
			code, message := payload.Code, payload.Message
			if payload.Error != nil {
				code, message = payload.Error.Code, payload.Error.Message
			}
			if payload.Response != nil && payload.Response.Error != nil {
				code = payload.Response.Error.Code
				message = payload.Response.Error.Message
			}
			return &responseStreamError{event: typ, code: code, message: message}
		}
		eventName = ""
		data = ""
		return nil
	}

	done := false
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return createResponseResponse{}, err
			}
			done = completed != nil
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if chunk == "[DONE]" {
				if err := dispatch(); err != nil {
					return createResponseResponse{}, err
				}
				if completed == nil {
					completed = &createResponseResponse{Status: "completed"}
					mergeCompletedOutputItems(completed, completedItems)
					if err := fillOutputFromDeltas(completed, streamed.String()); err != nil {
						return createResponseResponse{}, err
					}
				}
				done = true
				break
			}
			if data != "" {
				data += "\n"
			}
			data += chunk
		}
		if done {
			break
		}
	}
	if !done {
		if err := dispatch(); err != nil {
			return createResponseResponse{}, err
		}
		if err := scanner.Err(); err != nil {
			return createResponseResponse{}, fmt.Errorf("read responses stream: %w", err)
		}
	}
	if completed == nil {
		return createResponseResponse{}, errors.New("responses stream ended without response.completed")
	}
	return *completed, nil
}

type responseStreamError struct {
	event   string
	code    string
	message string
}

func (err *responseStreamError) Error() string {
	detail := err.code
	if err.message != "" {
		if detail != "" {
			detail += ": "
		}
		detail += err.message
	}
	if detail == "" {
		return "responses stream " + err.event
	}
	return "responses stream " + err.event + ": " + detail
}

func retryableResponseStreamError(err error) bool {
	var streamErr *responseStreamError
	if !errors.As(err, &streamErr) {
		return false
	}
	switch strings.ToLower(streamErr.code) {
	case "server_error", "rate_limit_error", "rate_limit_exceeded", "request_timeout", "overloaded_error":
		return true
	case "stream_read_error":
		return true
	case "":
		return streamErr.event == "error"
	default:
		return false
	}
}

func retryReasonForStreamError(err error) string {
	var streamErr *responseStreamError
	if errors.As(err, &streamErr) {
		switch strings.ToLower(streamErr.code) {
		case "server_error":
			return "stream_server_error"
		case "rate_limit_error", "rate_limit_exceeded":
			return "stream_rate_limit"
		case "request_timeout":
			return "stream_timeout"
		case "overloaded_error":
			return "stream_overloaded"
		case "stream_read_error":
			return "stream_read"
		default:
			return "stream_error"
		}
	}
	if strings.HasPrefix(err.Error(), "read responses stream:") {
		return "stream_read"
	}
	if strings.Contains(err.Error(), "ended without response.completed") {
		return "stream_incomplete"
	}
	return "stream_error"
}

func mergeCompletedOutputItems(resp *createResponseResponse, items map[int]json.RawMessage) {
	if resp == nil || len(items) == 0 {
		return
	}
	indexes := make([]int, 0, len(items))
	for index := range items {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		if index < len(resp.Output) {
			resp.Output[index] = items[index]
			continue
		}
		resp.Output = append(resp.Output, items[index])
	}
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
