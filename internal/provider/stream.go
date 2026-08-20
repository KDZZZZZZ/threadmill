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

	response, err := transport.do(ctx, body, "text/event-stream")
	if err != nil {
		return createResponseResponse{}, err
	}
	defer response.Body.Close()
	return readResponseStream(response.Body, sink)
}

func readResponseStream(r io.Reader, sink func(string)) (createResponseResponse, error) {
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
