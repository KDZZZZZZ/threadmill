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
	reasoningSink := event.ReasoningDeltaSink(ctx)
	retries := 0
	for {
		response, err := transport.do(ctx, body, "text/event-stream", &retries)
		if err != nil {
			return createResponseResponse{}, err
		}
		delivered := false
		type bufferedDelta struct {
			text string
			sink func(string)
		}
		var buffered []bufferedDelta
		forward := func(target func(string)) func(string) {
			if target == nil {
				return nil
			}
			return func(delta string) {
				if ctx.Err() != nil {
					return
				}
				if replayable {
					buffered = append(buffered, bufferedDelta{delta, target})
					return
				}
				delivered = true
				target(delta)
			}
		}
		result, readErr := readResponseStream(response.Body, forward(sink), activitySink, forward(reasoningSink))
		closeErr := response.Body.Close()
		if readErr == nil && closeErr == nil {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if result.Status != "completed" {
				return result, nil
			}
			for _, delta := range buffered {
				if err := ctx.Err(); err != nil {
					return result, err
				}
				delta.sink(delta.text)
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
		if (delivered && event.DeltaResetSink(ctx) == nil) || ctx.Err() != nil || retries >= transport.maxRetries || !retryable {
			return result, readErr
		}
		if delivered {
			event.DeltaResetSink(ctx)()
		}
		retries++
		notifyRetry(ctx, retryReasonForStreamError(readErr))
		if err := waitRetry(ctx, retryDelay(response, transport.retryInterval)); err != nil {
			return createResponseResponse{}, err
		}
	}
}

func readResponseStream(r io.Reader, sink func(string), activity func(bool), reasoningSink func(string)) (createResponseResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxResponseBody)

	var eventName, data string
	var completed *createResponseResponse
	completedItems := make(map[int]json.RawMessage)
	var streamed strings.Builder
	reasoningParts := make(map[[2]int]*strings.Builder)
	emitReasoning := func(index [2]int, text string, complete bool) error {
		if reasoningSink == nil || text == "" {
			return nil
		}
		part := reasoningParts[index]
		if part == nil {
			part = &strings.Builder{}
			reasoningParts[index] = part
		}
		if complete {
			if !strings.HasPrefix(text, part.String()) {
				return errors.New("completed reasoning text does not match streamed prefix")
			}
			text = text[part.Len():]
		}
		if text != "" {
			part.WriteString(text)
			reasoningSink(text)
		}
		return nil
	}
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
			Type         string                  `json:"type"`
			Code         string                  `json:"code"`
			Message      string                  `json:"message"`
			Delta        string                  `json:"delta"`
			Text         string                  `json:"text"`
			ContentIndex int                     `json:"content_index"`
			OutputIndex  *int                    `json:"output_index"`
			Item         json.RawMessage         `json:"item"`
			Response     *createResponseResponse `json:"response"`
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
		// The protocol distinguishes plaintext reasoning from reasoning_summary_text and encrypted_content.
		// Source: https://developers.openai.com/api/reference/resources/responses/streaming-events#response.reasoning_text.delta
		case "response.reasoning_text.delta", "response.reasoning_text.done":
			index := [2]int{0, payload.ContentIndex}
			if payload.OutputIndex != nil {
				index[0] = *payload.OutputIndex
			}
			text := payload.Delta
			complete := typ == "response.reasoning_text.done"
			if complete {
				text = payload.Text
			}
			if err := emitReasoning(index, text, complete); err != nil {
				return err
			}
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
	if completed.Status == "completed" && reasoningSink != nil {
		for outputIndex, rawOutput := range completed.Output {
			var output responseOutput
			if err := json.Unmarshal(rawOutput, &output); err != nil {
				return createResponseResponse{}, fmt.Errorf("decode responses output item: %w", err)
			}
			if output.Type != "reasoning" {
				continue
			}
			for contentIndex, content := range output.Content {
				if content.Type == "reasoning_text" {
					if err := emitReasoning([2]int{outputIndex, contentIndex}, content.Text, true); err != nil {
						return createResponseResponse{}, err
					}
				}
			}
		}
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
	if rateLimitedStreamError(streamErr) {
		return true
	}
	switch strings.ToLower(streamErr.code) {
	case "server_error", "request_timeout", "overloaded_error":
		return true
	case "stream_read_error":
		return true
	case "upstream_error":
		message := strings.ToLower(streamErr.message)
		return strings.HasPrefix(message, "openai stream disconnected before completion:") &&
			strings.HasSuffix(message, "unexpected eof")
	case "new_api_error":
		message := strings.ToLower(streamErr.message)
		return strings.HasPrefix(message, "no available channel for model ") ||
			strings.HasPrefix(message, "system cpu overloaded")
	case "":
		return streamErr.event == "error"
	default:
		return false
	}
}

func rateLimitedStreamError(err *responseStreamError) bool {
	switch strings.ToLower(err.code) {
	case "rate_limit_error", "rate_limit_exceeded", "gateway_concurrency_limit":
		return true
	case "upstream_error", "invalid_request":
		// These gateways wrap explicit quota failures in otherwise permanent codes.
		message := strings.ToLower(err.message)
		return strings.Contains(message, "too many rate-limited requests") ||
			strings.Contains(strings.ReplaceAll(message, " ", ""), "超过rpm限制")
	default:
		return false
	}
}

func retryReasonForStreamError(err error) string {
	var streamErr *responseStreamError
	if errors.As(err, &streamErr) {
		if rateLimitedStreamError(streamErr) {
			return "stream_rate_limit"
		}
		switch strings.ToLower(streamErr.code) {
		case "server_error":
			return "stream_server_error"
		case "request_timeout":
			return "stream_timeout"
		case "overloaded_error", "new_api_error":
			return "stream_overloaded"
		case "stream_read_error", "upstream_error":
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
