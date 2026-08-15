//go:build integration

package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/logging"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const reactTestInput = "THREADMILL_REACT_OK_42"

// echoUserInputTool 回显模型从用户消息中提取的 text 参数。
type echoUserInputTool struct {
	calls int
	input string
}

func (tool *echoUserInputTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        "echo_user_input",
		Description: "Echo the text argument verbatim. Use it when the user asks to verify a value through a tool.",
		InputSchema: json.RawMessage(`{
  "type":"object",
  "properties":{"text":{"type":"string"}},
  "required":["text"],
  "additionalProperties":false
}`),
	}
}

func (tool *echoUserInputTool) Execute(_ context.Context, call agenttool.Call) (agenttool.Output, error) {
	var arguments struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		return agenttool.Output{}, fmt.Errorf("decode echo arguments: %w", err)
	}
	if arguments.Text == "" {
		return agenttool.Output{}, errors.New("echo text is empty")
	}
	tool.calls++
	tool.input = arguments.Text
	return agenttool.Output{Content: arguments.Text}, nil
}

func TestLiveReActWithUserInputAndTool(t *testing.T) {
	if os.Getenv("OPENCODE_API_KEY") == "" {
		t.Skip("OPENCODE_API_KEY is required for the live integration test")
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate integration test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "../.."))
	cfg, err := provider.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	llm, err := provider.NewResponses(cfg.LLM, nil)
	if err != nil {
		t.Fatal(err)
	}

	logger := logging.New(logging.Config{Level: slog.LevelDebug})
	tool := &echoUserInputTool{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var turnResult agent.TurnResult
	loop, err := agent.NewLoop(agent.Config{
		Provider:      llm,
		ContextWindow: cfg.LLM.ContextWindow,
		Tools:         []agenttool.Tool{tool},
		Hooks: agent.Hooks{
			BeforeModel: []agent.BeforeModelHook{
				func(ctx context.Context, request agent.Request) error {
					logger.InfoContext(ctx, "model request",
						"messages", len(request.Messages),
						"tools", len(request.Tools),
					)
					return nil
				},
			},
			AfterModel: []agent.AfterModelHook{
				func(
					ctx context.Context,
					_ agent.Request,
					response agent.AssistantMessage,
					providerErr error,
				) error {
					if providerErr != nil {
						logger.ErrorContext(ctx, "model request failed", "error", providerErr)
						return nil
					}
					totalTokens := 0
					if response.Usage != nil {
						totalTokens = response.Usage.TotalTokens
					}
					logger.InfoContext(ctx, "model response",
						"tool_calls", len(response.ToolCalls),
						"total_tokens", totalTokens,
					)
					return nil
				},
			},
			BeforeTool: []agent.BeforeToolHook{
				func(ctx context.Context, call agenttool.Call) error {
					logger.InfoContext(ctx, "tool call",
						"name", call.Name,
						"call_id", call.ID,
					)
					return nil
				},
			},
			AfterTool: []agent.AfterToolHook{
				func(ctx context.Context, call agenttool.Call, result agenttool.Result) error {
					logger.InfoContext(ctx, "tool result",
						"name", call.Name,
						"call_id", call.ID,
						"is_error", result.IsError,
					)
					return nil
				},
			},
			AfterTurn: []agent.AfterTurnHook{
				func(_ context.Context, _ agent.UserMessage, result agent.TurnResult) error {
					turnResult = result
					cancel()
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	loop.Enqueue(agent.UserMessage{Content: `请务必调用 echo_user_input 工具，把 text 参数设置为 "` + reactTestInput + `"。读取工具结果后，最终只回复工具返回值。`})
	runErr := loop.Run(ctx)
	if turnResult.Err != nil {
		t.Fatalf("ReAct turn failed: %v", turnResult.Err)
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled after completed turn", runErr)
	}
	if turnResult.Steps < 2 {
		t.Fatalf("model steps = %d, want at least 2", turnResult.Steps)
	}
	if tool.calls == 0 || tool.input != reactTestInput {
		t.Fatalf("tool calls = %d, input = %q", tool.calls, tool.input)
	}

	history := loop.Messages()
	if len(history) < 4 {
		t.Fatalf("message count = %d, want at least 4", len(history))
	}
	var hasToolResult bool
	for _, message := range history {
		if message.Role == agent.RoleTool &&
			message.ToolResult != nil &&
			message.ToolResult.Content == reactTestInput &&
			!message.ToolResult.IsError {
			hasToolResult = true
			break
		}
	}
	if !hasToolResult {
		t.Fatal("history has no successful echo tool result")
	}

	final := history[len(history)-1]
	if final.Role != agent.RoleAssistant || len(final.ToolCalls) != 0 {
		t.Fatalf("final message = %#v", final)
	}
	if !strings.Contains(final.Content, reactTestInput) {
		t.Fatalf("final content = %q, want marker %q", final.Content, reactTestInput)
	}
}
