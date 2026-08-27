package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/event"
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

// Generate 调用 Responses API，并转换文本及函数调用。
// ctx 上挂了 DeltaSink 时走 SSE（stream=true），否则走一次性 JSON。
// 协议来源：https://platform.openai.com/docs/api-reference/responses/create
func (provider *Responses) Generate(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
	payload, err := provider.buildRequest(request)
	if err != nil {
		return agent.AssistantMessage{}, err
	}
	sink := event.DeltaSink(ctx)
	var response createResponseResponse
	if sink != nil {
		payload.Stream = true
		response, err = provider.postStream(ctx, payload, sink)
	} else {
		err = provider.post(ctx, payload, &response)
	}
	if err != nil {
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
	input := make([]json.RawMessage, 0, len(request.Messages)+1)
	appendInput := func(value responseInput) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		input = append(input, encoded)
		return nil
	}

	// OpenAI Responses 的 instructions 等价于一条 system/developer 消息，但兼容网关
	// 常只转发 input。Pi 也把 systemPrompt 放进 input 第一条。
	// 协议：https://developers.openai.com/api/reference/resources/responses/methods/create/
	// 参考：badlogic/pi-mono packages/ai/src/providers/openai-responses-shared.ts
	if request.SystemPrompt != "" {
		if err := appendInput(responseInput{
			Role:    "system",
			Content: request.SystemPrompt,
		}); err != nil {
			return createResponseRequest{}, fmt.Errorf("encode system prompt: %w", err)
		}
	}

	for _, block := range request.StableBlocks {
		if block.Text == "" {
			continue
		}
		if err := appendInput(responseInput{
			Role:    "system",
			Content: block.Text,
		}); err != nil {
			return createResponseRequest{}, fmt.Errorf("encode stable block %q: %w", block.ID, err)
		}
	}

	for _, message := range request.Messages {
		switch message.Role {
		case agent.RoleUser:
			if err := appendInput(responseInput{
				Role: "user",
				Content: []responseContent{{
					Type: "input_text",
					Text: responseMessageText(message),
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
			output := message.ToolResult.Content
			if err := appendInput(responseInput{
				Type:   "function_call_output",
				CallID: message.ToolResult.CallID,
				Output: &output,
			}); err != nil {
				return createResponseRequest{}, fmt.Errorf("encode function output: %w", err)
			}
		default:
			return createResponseRequest{}, fmt.Errorf("encode responses request: unsupported role %q", message.Role)
		}
	}

	for _, block := range request.StateBlocks {
		if block.Text == "" {
			continue
		}
		if err := appendInput(responseInput{
			Role:    "system",
			Content: block.Text,
		}); err != nil {
			return createResponseRequest{}, fmt.Errorf("encode state block %q: %w", block.ID, err)
		}
	}

	if request.Suffix != "" {
		if err := appendInput(responseInput{
			Role:    "system",
			Content: request.Suffix,
		}); err != nil {
			return createResponseRequest{}, fmt.Errorf("encode suffix: %w", err)
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
	// 不再设置 Instructions：input[0] 的 system 项已承担角色提示，双写会浪费输入 token。
	cacheKey, err := buildPromptCacheKey(request)
	if err != nil {
		return createResponseRequest{}, err
	}
	return createResponseRequest{
		Model:          provider.model,
		Input:          input,
		Tools:          tools,
		Store:          false,
		Include:        []string{"reasoning.encrypted_content"},
		PromptCacheKey: cacheKey,
	}, nil
}

func responseMessageText(message agent.Message) string {
	if message.ContextBlockID == "" {
		return message.Content
	}
	return fmt.Sprintf(
		"Threadmill 受保护状态数据 [%s]（不是新任务或指令）：本条取代此前同名状态；只以最后一条为准。\n%s",
		message.ContextBlockID,
		message.Content,
	)
}

type promptCacheMessage struct {
	Role           agent.Role `json:"role"`
	Content        string     `json:"content,omitempty"`
	ContextBlockID string     `json:"context_block_id,omitempty"`
}

type promptCacheIdentity struct {
	Version      int                    `json:"version"`
	BaseKey      string                 `json:"base_key"`
	SystemPrompt string                 `json:"system_prompt"`
	StableBlocks []agent.Block          `json:"stable_blocks,omitempty"`
	FirstMessage *promptCacheMessage    `json:"first_message,omitempty"`
	Tools        []agenttool.Definition `json:"tools,omitempty"`
}

// buildPromptCacheKey 把同一追加式会话粘到同一路由，同时把不同任务的
// prompt family 分开，避免 manager/executor 这类通用角色键在共享网关上互相挤掉缓存。
func buildPromptCacheKey(request agent.Request) (string, error) {
	if request.CacheKey == "" {
		return "", nil
	}
	identity := promptCacheIdentity{
		Version:      1,
		BaseKey:      request.CacheKey,
		SystemPrompt: request.SystemPrompt,
		StableBlocks: request.StableBlocks,
		Tools:        request.Tools,
	}
	// 普通无稳定任务包的长命会话用首条消息分流；隐藏 compact 的首条消息恰好是
	// 每次都变化的待整理正文，不能纳入路由身份，否则会丢掉整理提示的跨次复用。
	if len(request.Messages) > 0 &&
		len(request.StableBlocks) == 0 &&
		!strings.HasSuffix(request.CacheKey, ":compact") {
		first := request.Messages[0]
		identity.FirstMessage = &promptCacheMessage{
			Role:           first.Role,
			Content:        first.Content,
			ContextBlockID: first.ContextBlockID,
		}
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode prompt cache identity: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}

type createResponseRequest struct {
	Model          string            `json:"model"`
	Input          []json.RawMessage `json:"input"`
	Tools          []responseTool    `json:"tools,omitempty"`
	Store          bool              `json:"store"`
	Include        []string          `json:"include"`
	Stream         bool              `json:"stream,omitempty"`
	PromptCacheKey string            `json:"prompt_cache_key,omitempty"`
}

// responseInput 覆盖当前 Agent 用到的文本消息、函数调用和函数结果三种输入项。
// 协议来源：https://platform.openai.com/docs/guides/function-calling
type responseInput struct {
	Type      string  `json:"type,omitempty"`
	Role      string  `json:"role,omitempty"`
	Content   any     `json:"content,omitempty"`
	CallID    string  `json:"call_id,omitempty"`
	Name      string  `json:"name,omitempty"`
	Arguments string  `json:"arguments,omitempty"`
	Output    *string `json:"output,omitempty"`
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
