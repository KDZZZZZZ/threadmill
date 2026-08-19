// Package tool 定义 ReAct 循环与工具实现之间的最小协议。
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrInvalidDefinition 表示工具定义不能安全地提供给模型。
	ErrInvalidDefinition = errors.New("tool: invalid definition")
	// ErrInvalidCall 表示模型生成了无法调度的工具调用。
	ErrInvalidCall = errors.New("tool: invalid call")
)

// Definition 是提供给模型的工具元信息，InputSchema 必须是 JSON Schema 对象。
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Validate 校验工具名、描述和输入 JSON Schema 的基础结构。
func (d Definition) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("%w: missing name", ErrInvalidDefinition)
	}
	if strings.TrimSpace(d.Description) == "" {
		return fmt.Errorf("%w: missing description", ErrInvalidDefinition)
	}
	if !isJSONObject(d.InputSchema) {
		return fmt.Errorf("%w: input schema must be a json object", ErrInvalidDefinition)
	}
	return nil
}

// Call 是模型生成的一次工具调用，ID 用于关联对应的 Result。
type Call struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Validate 校验调用标识、工具名和参数的基础结构。空参数等价于空对象。
func (c Call) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return fmt.Errorf("%w: missing call id", ErrInvalidCall)
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("%w: missing tool name", ErrInvalidCall)
	}
	if len(c.Arguments) > 0 && !isJSONObject(c.Arguments) {
		return fmt.Errorf("%w: arguments must be a json object", ErrInvalidCall)
	}
	return nil
}

// Output 是工具成功执行后返回给 ReAct 循环的模型可见内容。
type Output struct {
	Content string
	Details json.RawMessage
}

// Result 是 ReAct 循环写入对话记录的工具结果。
// Tool 实现只返回 Output 和 error，调用关联及错误归一化由循环负责。
// Details 供日志或 UI 使用，不发给模型。
type Result struct {
	CallID  string          `json:"tool_call_id"`
	Name    string          `json:"name"`
	Content string          `json:"content"`
	Details json.RawMessage `json:"details,omitempty"`
	IsError bool            `json:"is_error"`
}

// Tool 提供模型可见定义，并执行一次支持上下文取消的工具调用。
type Tool interface {
	Definition() Definition
	Execute(ctx context.Context, call Call) (Output, error)
}

// isJSONObject 判断数据是否为非 null 的 JSON 对象。
func isJSONObject(data []byte) bool {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		return false
	}
	return value != nil
}
