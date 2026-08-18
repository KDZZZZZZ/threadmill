package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

var (
	// ErrNilTool 表示工具列表包含 nil。
	ErrNilTool = errors.New("agent: nil tool")
	// ErrDuplicateTool 表示多个工具使用了同一个名称。
	ErrDuplicateTool = errors.New("agent: duplicate tool")
)

// prepareTools 校验并复制注册到 Agent 的工具及其模型定义。
func prepareTools(registeredTools []agenttool.Tool) (
	map[string]agenttool.Tool,
	[]agenttool.Definition,
	error,
) {
	tools := make(map[string]agenttool.Tool, len(registeredTools))
	definitions := make([]agenttool.Definition, 0, len(registeredTools))
	for i, registered := range registeredTools {
		if registered == nil {
			return nil, nil, fmt.Errorf("%w at index %d", ErrNilTool, i)
		}
		definition := cloneDefinition(registered.Definition())
		if err := definition.Validate(); err != nil {
			return nil, nil, fmt.Errorf("registering tool at index %d: %w", i, err)
		}
		if _, exists := tools[definition.Name]; exists {
			return nil, nil, fmt.Errorf("%w: %s", ErrDuplicateTool, definition.Name)
		}
		tools[definition.Name] = registered
		if toolHidden(registered) {
			continue
		}
		definitions = append(definitions, definition)
	}
	return tools, definitions, nil
}

// validateToolCallIDs 在记录助手消息前确保调用标识在整个 ReAct 内唯一。
func (l *Loop) validateToolCallIDs(calls []agenttool.Call) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	pending := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.ID) == "" {
			return fmt.Errorf("%w: missing call id", agenttool.ErrInvalidCall)
		}
		if _, exists := l.usedToolCallIDs[call.ID]; exists {
			return fmt.Errorf("%w: duplicate call id %q", agenttool.ErrInvalidCall, call.ID)
		}
		if _, exists := pending[call.ID]; exists {
			return fmt.Errorf("%w: duplicate call id %q", agenttool.ErrInvalidCall, call.ID)
		}
		pending[call.ID] = struct{}{}
	}
	if l.usedToolCallIDs == nil {
		l.usedToolCallIDs = make(map[string]struct{}, len(calls))
	}
	for id := range pending {
		l.usedToolCallIDs[id] = struct{}{}
	}
	return nil
}

// executeToolCalls 串行执行模型生成的工具调用，并为每个调用写入关联结果。
func (l *Loop) executeToolCalls(ctx context.Context, calls []agenttool.Call) error {
	for i, original := range calls {
		call := cloneCall(original)
		if len(call.Arguments) == 0 {
			call.Arguments = json.RawMessage(`{}`)
		}

		if err := ctx.Err(); err != nil {
			if hookErr := l.appendCanceledResults(ctx, calls[i:]); hookErr != nil {
				return hookErr
			}
			return err
		}

		result, hookErr := l.executeToolCall(ctx, call)
		if err := l.appendToolResult(result); err != nil {
			return err
		}
		if hookErr != nil {
			return hookErr
		}

		if err := ctx.Err(); err != nil {
			if hookErr := l.appendCanceledResults(ctx, calls[i+1:]); hookErr != nil {
				return hookErr
			}
			return err
		}
	}
	return nil
}

// executeToolCall 校验并执行一个工具调用，然后触发对应的前后钩子。
func (l *Loop) executeToolCall(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
	result := agenttool.Result{CallID: call.ID, Name: call.Name}
	registered, exists := l.tools[call.Name]
	validationErr := call.Validate()

	switch {
	case validationErr != nil:
		result.Content = validationErr.Error()
		result.IsError = true
	case !exists:
		result.Content = fmt.Sprintf("tool %q not found", call.Name)
		result.IsError = true
	default:
		if err := l.hooks.beforeTool(ctx, call); err != nil {
			result.Content = err.Error()
			result.IsError = true
			return result, l.hooks.afterTool(ctx, call, result)
		}
		if err := ctx.Err(); err != nil {
			result.Content = err.Error()
			result.IsError = true
			return result, l.hooks.afterTool(ctx, call, result)
		}

		toolCtx := ctx
		if toolHidden(registered) {
			toolCtx = l.withTranscript(ctx)
		}
		output, err := registered.Execute(toolCtx, cloneCall(call))
		if err != nil {
			result.Content = err.Error()
			result.IsError = true
		} else {
			result.Content = output.Content
			result.Details = bytes.Clone(output.Details)
		}
	}

	return result, l.hooks.afterTool(ctx, call, result)
}

// appendCanceledResults 为取消后未执行的调用补齐结果，保持模型对话协议完整。
func (l *Loop) appendCanceledResults(ctx context.Context, calls []agenttool.Call) error {
	var hookErr error
	for _, original := range calls {
		call := cloneCall(original)
		if len(call.Arguments) == 0 {
			call.Arguments = json.RawMessage(`{}`)
		}
		result := agenttool.Result{
			CallID:  call.ID,
			Name:    call.Name,
			Content: "tool call canceled before execution",
			IsError: true,
		}
		if err := l.appendToolResult(result); err != nil {
			return err
		}
		hookErr = errors.Join(hookErr, l.hooks.afterTool(ctx, call, result))
	}
	return hookErr
}

// cloneCalls 深拷贝工具调用及其 JSON 参数。
func cloneCalls(calls []agenttool.Call) []agenttool.Call {
	cloned := make([]agenttool.Call, len(calls))
	for i, call := range calls {
		cloned[i] = cloneCall(call)
	}
	return cloned
}

// cloneCall 深拷贝单个工具调用的 JSON 参数。
func cloneCall(call agenttool.Call) agenttool.Call {
	cloned := call
	cloned.Arguments = bytes.Clone(call.Arguments)
	return cloned
}

// cloneDefinition 深拷贝工具定义中的 JSON Schema。
func cloneDefinition(definition agenttool.Definition) agenttool.Definition {
	cloned := definition
	cloned.InputSchema = bytes.Clone(definition.InputSchema)
	return cloned
}
