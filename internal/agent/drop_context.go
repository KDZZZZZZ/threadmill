package agent

import (
	"context"
	"encoding/json"
	"fmt"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const dropContextPressureReminder = "上下文已接近窗口上限。请调用 " + memoryDropFromContextToolName + " 丢掉暂时用不到的节点详情；这不会修改记忆图。"

type dropFromContextTool struct {
	loop *Loop
}

var _ agenttool.Tool = (*dropFromContextTool)(nil)

// DropFromContextTool 从本 Agent 对话里去掉指定节点的详情，不改记忆图。
func DropFromContextTool(loop *Loop) agenttool.Tool {
	return &dropFromContextTool{loop: loop}
}

func (*dropFromContextTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        memoryDropFromContextToolName,
		Description: "从本轮对话上下文中去掉指定记忆节点的详细内容，减轻窗口压力。不删除记忆图上的节点；之后仍可用工具再查到它们。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"node_ids":{"type":"array","items":{"type":"string"},"description":"要从对话上下文中丢掉详情的节点 ID"}},"required":["node_ids"],"additionalProperties":false}`),
	}
}

func (t *dropFromContextTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.loop == nil {
		return agenttool.Output{}, fmt.Errorf("%s: nil loop", memoryDropFromContextToolName)
	}

	var args struct {
		NodeIDs []string `json:"node_ids"`
	}
	raw := call.Arguments
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return agenttool.Output{}, fmt.Errorf("decode arguments: %w", err)
	}
	ids := uniqueIDs(args.NodeIDs)
	if len(ids) == 0 {
		return agenttool.Output{}, fmt.Errorf("%s: missing node_ids", memoryDropFromContextToolName)
	}

	drop := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		drop[id] = struct{}{}
	}
	t.loop.dropNodesFromMessages(drop)

	payload, err := json.Marshal(struct {
		Dropped []string `json:"dropped"`
	}{Dropped: ids})
	if err != nil {
		return agenttool.Output{}, fmt.Errorf("encode dropped nodes: %w", err)
	}
	return agenttool.Output{Content: string(payload)}, nil
}

func (l *Loop) dropNodesFromMessages(ids map[string]struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.messages {
		l.messages[i].Content = dropNodesFromJSON(l.messages[i].Content, ids)
		if l.messages[i].ToolResult == nil {
			continue
		}
		result := *l.messages[i].ToolResult
		result.Content = dropNodesFromJSON(result.Content, ids)
		if len(result.Details) > 0 {
			result.Details = json.RawMessage(dropNodesFromJSON(string(result.Details), ids))
		}
		l.messages[i].ToolResult = &result
	}
}

// RemindDropContextOnPressure 在请求接近上下文窗口时提醒模型调用丢弃工具。
// 提醒写进 Suffix 段（wire 末尾），不碰静态前缀和历史。
func RemindDropContextOnPressure(loop *Loop) AssembleRequestHook {
	return func(_ context.Context, request Request) (Request, error) {
		if loop == nil || loop.contextWindow <= 0 {
			return request, nil
		}
		reminder := loop.dropContextReminderText()
		if request.Suffix == reminder {
			return request, nil
		}
		if estimateRequestTokens(request) < softContextThreshold(loop.contextWindow) {
			return request, nil
		}
		request.Suffix = reminder
		return request, nil
	}
}

func (l *Loop) dropContextReminderText() string {
	if l != nil && l.dropContextReminder != "" {
		return l.dropContextReminder
	}
	return dropContextPressureReminder
}

func softContextThreshold(contextWindow int) int {
	return contextWindow * 3 / 4
}

func estimateRequestTokens(request Request) int {
	total := estimateTextTokens(request.WirePrompt())
	for _, message := range request.Messages {
		total += estimateTokens(message)
	}
	return total
}

func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

func dropNodesFromJSON(raw string, ids map[string]struct{}) string {
	if raw == "" || len(ids) == 0 {
		return raw
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	next, changed := dropNodesValue(value, ids)
	if !changed {
		return raw
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func dropNodesValue(value any, ids map[string]struct{}) (any, bool) {
	switch typed := value.(type) {
	case []any:
		out := make([]any, 0, len(typed))
		changed := false
		for _, item := range typed {
			if id, ok := objectID(item); ok {
				if _, drop := ids[id]; drop {
					changed = true
					continue
				}
			}
			next, childChanged := dropNodesValue(item, ids)
			changed = changed || childChanged
			out = append(out, next)
		}
		return out, changed
	case map[string]any:
		changed := false
		for key, child := range typed {
			next, childChanged := dropNodesValue(child, ids)
			if !childChanged {
				continue
			}
			typed[key] = next
			changed = true
		}
		return typed, changed
	default:
		return value, false
	}
}

func objectID(value any) (string, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return "", false
	}
	id, ok := object["id"].(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
