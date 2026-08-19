package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

var _ agent.Provider = (*Responses)(nil)

// Responses 通过 OpenAI-compatible Responses API 生成下一条助手消息。
type Responses struct {
	transport
}

// NewResponses 校验配置、解析环境变量中的密钥并创建 Provider。
func NewResponses(config LLMConfig, client *http.Client) (*Responses, error) {
	transport, err := newTransport(config, OpenAIResponses, "/responses", client)
	if err != nil {
		return nil, err
	}
	return &Responses{transport: transport}, nil
}

// Generate 调用非流式 Responses API，并转换文本及函数调用。
// 协议来源：https://platform.openai.com/docs/api-reference/responses/create
func (provider *Responses) Generate(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
	payload, err := provider.buildRequest(request)
	if err != nil {
		return agent.AssistantMessage{}, err
	}
	var response createResponseResponse
	if err := provider.post(ctx, payload, &response); err != nil {
		return agent.AssistantMessage{}, err
	}
	message, err := response.assistantMessage()
	if err != nil {
		return agent.AssistantMessage{}, err
	}
	message.API = OpenAIResponses
	message.Provider = OpenAIResponses
	message.Model = provider.model
	return message, nil
}

// buildRequest 将 Agent 的对话和工具定义转换为 Responses API 输入。
func (provider *Responses) buildRequest(request agent.Request) (createResponseRequest, error) {
	input := make([]json.RawMessage, 0, len(request.Messages))
	appendInput := func(value responseInput) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		input = append(input, encoded)
		return nil
	}

	for _, message := range request.Messages {
		switch message.Role {
		case agent.RoleUser:
			if err := appendInput(responseInput{
				Role: "user",
				Content: []responseContent{{
					Type: "input_text",
					Text: message.Content,
				}},
			}); err != nil {
				return createResponseRequest{}, fmt.Errorf("encode user message: %w", err)
			}
		case agent.RoleAssistant:
			if len(message.ModelData) > 0 {
				var items []json.RawMessage
				if err := json.Unmarshal(message.ModelData, &items); err != nil || items == nil {
					return createResponseRequest{}, errors.New("encode responses request: invalid assistant model data")
				}
				input = append(input, items...)
				continue
			}
			if message.Content != "" {
				if err := appendInput(responseInput{Role: "assistant", Content: message.Content}); err != nil {
					return createResponseRequest{}, fmt.Errorf("encode assistant message: %w", err)
				}
			}
			for _, call := range message.ToolCalls {
				arguments := call.Arguments
				if len(arguments) == 0 {
					arguments = json.RawMessage(`{}`)
				}
				if err := appendInput(responseInput{
					Type:      "function_call",
					CallID:    call.ID,
					Name:      call.Name,
					Arguments: string(arguments),
				}); err != nil {
					return createResponseRequest{}, fmt.Errorf("encode function call: %w", err)
				}
			}
		case agent.RoleTool:
			if message.ToolResult == nil {
				return createResponseRequest{}, errors.New("encode responses request: tool message has no result")
			}
			if err := appendInput(responseInput{
				Type:   "function_call_output",
				CallID: message.ToolResult.CallID,
				Output: message.ToolResult.Content,
			}); err != nil {
				return createResponseRequest{}, fmt.Errorf("encode function output: %w", err)
			}
		default:
			return createResponseRequest{}, fmt.Errorf("encode responses request: unsupported role %q", message.Role)
		}
	}

	tools := make([]responseTool, len(request.Tools))
	for i, definition := range request.Tools {
		if err := definition.Validate(); err != nil {
			return createResponseRequest{}, fmt.Errorf("encode responses tool %q: %w", definition.Name, err)
		}
		var parameters map[string]any
		if err := json.Unmarshal(definition.InputSchema, &parameters); err != nil {
			return createResponseRequest{}, fmt.Errorf("encode responses tool %q: %w", definition.Name, err)
		}
		tools[i] = responseTool{
			Type:        "function",
			Name:        definition.Name,
			Description: definition.Description,
			Parameters:  parameters,
			Strict:      false,
		}
	}

	// store=false 时回放加密 reasoning 项，保持工具调用后的无状态续接。
	// 协议来源：https://platform.openai.com/docs/guides/conversation-state
	return createResponseRequest{
		Model:        provider.model,
		Instructions: request.SystemPrompt,
		Input:        input,
		Tools:        tools,
		Store:        false,
		Include:      []string{"reasoning.encrypted_content"},
	}, nil
}

type createResponseRequest struct {
	Model        string            `json:"model"`
	Instructions string            `json:"instructions,omitempty"`
	Input        []json.RawMessage `json:"input"`
	Tools        []responseTool    `json:"tools,omitempty"`
	Store        bool              `json:"store"`
	Include      []string          `json:"include"`
}

// responseInput 覆盖当前 Agent 用到的文本消息、函数调用和函数结果三种输入项。
// 协议来源：https://platform.openai.com/docs/guides/function-calling
type responseInput struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   any    `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type responseContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal,omitempty"`
}

type responseTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

type createResponseResponse struct {
	Status            string            `json:"status"`
	Error             *responseError    `json:"error"`
	IncompleteDetails *incomplete       `json:"incomplete_details"`
	Output            []json.RawMessage `json:"output"`
	Usage             *responseUsage    `json:"usage"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type incomplete struct {
	Reason string `json:"reason"`
}

// responseUsage 对应 Responses API 的原始 usage 对象。
// 协议来源：https://platform.openai.com/docs/api-reference/responses/object#responses-object-usage
type responseUsage struct {
	InputTokens  int `json:"input_tokens"`
	InputDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens  int `json:"output_tokens"`
	OutputDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

// agentUsage 将协议字段原样映射为 Agent 的单次请求用量。
func (usage responseUsage) agentUsage() *agent.Usage {
	return &agent.Usage{
		InputTokens:      usage.InputTokens,
		CachedTokens:     usage.InputDetails.CachedTokens,
		CacheWriteTokens: usage.InputDetails.CacheWriteTokens,
		OutputTokens:     usage.OutputTokens,
		ReasoningTokens:  usage.OutputDetails.ReasoningTokens,
		TotalTokens:      usage.TotalTokens,
	}
}

type responseOutput struct {
	Type      string            `json:"type"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments string            `json:"arguments"`
	Content   []responseContent `json:"content"`
	Summary   []responseContent `json:"summary"`
}

// assistantMessage 提取所有 output_text，并保留输出顺序中的函数调用。
func (response createResponseResponse) assistantMessage() (agent.AssistantMessage, error) {
	var message agent.AssistantMessage
	if response.Usage != nil {
		message.Usage = response.Usage.agentUsage()
	}
	if response.Status != "completed" {
		detail := response.Status
		if response.Error != nil && response.Error.Message != "" {
			detail += ": " + response.Error.Message
		} else if response.IncompleteDetails != nil && response.IncompleteDetails.Reason != "" {
			detail += ": " + response.IncompleteDetails.Reason
		}
		return message, fmt.Errorf("responses generation did not complete: %s", detail)
	}
	if len(response.Output) > 0 {
		modelData, err := json.Marshal(response.Output)
		if err != nil {
			return message, fmt.Errorf("preserve responses output: %w", err)
		}
		message.ModelData = modelData
	}

	var text strings.Builder
	for _, rawOutput := range response.Output {
		var output responseOutput
		if err := json.Unmarshal(rawOutput, &output); err != nil {
			return message, fmt.Errorf("decode responses output item: %w", err)
		}
		switch output.Type {
		case "message":
			for _, content := range output.Content {
				switch content.Type {
				case "output_text":
					text.WriteString(content.Text)
				case "refusal":
					text.WriteString(content.Refusal)
				default:
					return message, fmt.Errorf(
						"unsupported responses message content type %q",
						content.Type,
					)
				}
			}
		case "function_call":
			message.ToolCalls = append(message.ToolCalls, agenttool.Call{
				ID:        output.CallID,
				Name:      output.Name,
				Arguments: json.RawMessage(output.Arguments),
			})
		case "reasoning":
			for _, part := range output.Summary {
				if text := strings.TrimSpace(part.Text); text == "" {
					continue
				}
				if message.Thinking != "" {
					message.Thinking += "\n"
				}
				message.Thinking += part.Text
			}
		default:
			return message, fmt.Errorf("unsupported responses output type %q", output.Type)
		}
	}
	message.Content = text.String()
	if message.Content == "" && len(message.ToolCalls) == 0 {
		return message, errors.New("completed responses generation has no assistant output")
	}
	if len(message.ToolCalls) > 0 {
		message.StopReason = agent.StopReasonToolUse
	} else {
		message.StopReason = agent.StopReasonStop
	}
	return message, nil
}
