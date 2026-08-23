package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const (
	compactMemoryToolName          = "compact_memory"
	injectSubscribedMemoryToolName = "inject_subscribed_memory"
)

type hidden interface {
	Hidden() bool
}

// Transcript 是隐藏记忆工具执行时需要的对话快照；由 Loop/Hook 注入 context。
type Transcript struct {
	Messages            []Message
	Subscribed          []string
	Provider            Provider
	ContextWindow       int
	AgentID             string
	CompactPrompt       string
	CompactJSONReminder string
}

type transcriptKey struct{}

// WithTranscript 把对话快照放进 ctx，供不导入 agent 循环状态的工具读取。
func WithTranscript(ctx context.Context, transcript Transcript) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, transcriptKey{}, transcript)
}

// TranscriptFromContext 返回 Loop 注入的对话快照。
func TranscriptFromContext(ctx context.Context) (Transcript, bool) {
	if ctx == nil {
		return Transcript{}, false
	}
	transcript, ok := ctx.Value(transcriptKey{}).(Transcript)
	return transcript, ok
}

func toolHidden(tool agenttool.Tool) bool {
	h, ok := tool.(hidden)
	return ok && h.Hidden()
}

type compactMemoryTool struct {
	memory env.MemoryView
}

var (
	_ agenttool.Tool      = compactMemoryTool{}
	_ agenttool.EnvBinder = compactMemoryTool{}
	_ hidden              = compactMemoryTool{}
)

func newCompactMemoryTool() compactMemoryTool {
	return compactMemoryTool{}
}

func (compactMemoryTool) Hidden() bool { return true }

func (compactMemoryTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        compactMemoryToolName,
		Description: "把旧对话整理进记忆图，只留下尾部消息。keep_recent_tokens 为 0 时不保留前缀。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"keep_recent_tokens":{"type":"integer","description":"保留的尾部 token 预算，0 表示全部写入记忆"}},"additionalProperties":false}`),
	}
}

func (t compactMemoryTool) BindEnv(e env.Env) agenttool.Tool {
	t.memory = e.Memory
	return t
}

func (t compactMemoryTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.memory == nil {
		return agenttool.Output{}, fmt.Errorf("%s: unbound memory", compactMemoryToolName)
	}
	transcript, ok := TranscriptFromContext(ctx)
	if !ok {
		return agenttool.Output{}, fmt.Errorf("%s: missing transcript", compactMemoryToolName)
	}

	var args struct {
		KeepRecentTokens int `json:"keep_recent_tokens"`
	}
	raw := call.Arguments
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return agenttool.Output{}, fmt.Errorf("decode arguments: %w", err)
	}

	graph, tail, err := CompactHistory(
		ctx,
		transcript.Provider,
		t.memory.Snapshot(),
		transcript.Messages,
		transcript.Subscribed,
		args.KeepRecentTokens,
		transcript.AgentID,
	)
	if err != nil {
		return agenttool.Output{}, err
	}
	if err := t.memory.Commit(graph); err != nil {
		return agenttool.Output{}, fmt.Errorf("commit compacted memory: %w", err)
	}
	details, err := json.Marshal(tail)
	if err != nil {
		return agenttool.Output{}, fmt.Errorf("encode compact tail: %w", err)
	}
	return agenttool.Output{Details: details}, nil
}

type injectSubscribedMemoryTool struct {
	memory env.MemoryView
}

var (
	_ agenttool.Tool      = injectSubscribedMemoryTool{}
	_ agenttool.EnvBinder = injectSubscribedMemoryTool{}
	_ hidden              = injectSubscribedMemoryTool{}
)

func newInjectSubscribedMemoryTool() injectSubscribedMemoryTool {
	return injectSubscribedMemoryTool{}
}

func (injectSubscribedMemoryTool) Hidden() bool { return true }

func (injectSubscribedMemoryTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        injectSubscribedMemoryToolName,
		Description: "把当前订阅子图里的节点格式化成系统提示片段。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

func (t injectSubscribedMemoryTool) BindEnv(e env.Env) agenttool.Tool {
	t.memory = e.Memory
	return t
}

func (t injectSubscribedMemoryTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.memory == nil {
		return agenttool.Output{}, fmt.Errorf("%s: unbound memory", injectSubscribedMemoryToolName)
	}
	transcript, ok := TranscriptFromContext(ctx)
	if !ok {
		return agenttool.Output{}, fmt.Errorf("%s: missing transcript", injectSubscribedMemoryToolName)
	}
	content := assembleSystemPrompt("", t.memory.Snapshot(), transcript.Subscribed)
	return agenttool.Output{Content: content}, nil
}

func hiddenMemoryTools() []agenttool.Tool {
	return []agenttool.Tool{
		newCompactMemoryTool(),
		newInjectSubscribedMemoryTool(),
	}
}

func keepRecentArgs(keep int) json.RawMessage {
	raw, err := json.Marshal(struct {
		KeepRecentTokens int `json:"keep_recent_tokens"`
	}{KeepRecentTokens: keep})
	if err != nil {
		return json.RawMessage(`{"keep_recent_tokens":0}`)
	}
	return raw
}

func unwrapEventProvider(p Provider) Provider {
	if wrapped, ok := p.(eventProvider); ok {
		return wrapped.inner
	}
	return p
}

// hiddenCostProvider keeps organizer calls out of the visible model stream while
// retaining their token and retry cost on the enclosing memory operation.
type hiddenCostProvider struct {
	inner    Provider
	tokens   int
	retries  int
	activity func(bool)
}

func (p *hiddenCostProvider) Generate(ctx context.Context, request Request) (AssistantMessage, error) {
	ctx = event.WithRetrySink(ctx, func(string) { p.retries++ })
	if p.activity != nil {
		ctx = event.WithDeltaActivitySink(ctx, p.activity)
	}
	message, err := p.inner.Generate(ctx, request)
	if message.Usage != nil {
		p.tokens += message.Usage.TotalTokens
	}
	return message, err
}

func (l *Loop) snapshotTranscript() Transcript {
	l.mu.Lock()
	defer l.mu.Unlock()
	subscribed := append([]string(nil), l.fixedSubscribedSubgraphs...)
	subscribed = uniqueIDs(append(subscribed, l.subscribedSubgraphs...))
	return Transcript{
		Messages:            cloneMessages(l.messages),
		Subscribed:          subscribed,
		Provider:            unwrapEventProvider(l.provider),
		ContextWindow:       l.contextWindow,
		AgentID:             l.agentID,
		CompactPrompt:       l.compactPrompt,
		CompactJSONReminder: l.compactJSONReminder,
	}
}

func (l *Loop) withTranscript(ctx context.Context) context.Context {
	return WithTranscript(ctx, l.snapshotTranscript())
}

func (l *Loop) execHidden(ctx context.Context, name string, args json.RawMessage) (out agenttool.Output, err error) {
	callID := "hidden-" + name
	started := time.Now()
	transcript := l.snapshotTranscript()
	cost := &hiddenCostProvider{inner: transcript.Provider}
	cost.activity = func(text bool) {
		l.publish(ctx, event.MemoryDelta(l.agentID, name, callID, text))
	}
	transcript.Provider = cost
	ctx = WithTranscript(ctx, transcript)
	l.publish(ctx, event.MemoryStart(l.agentID, name, callID))
	defer func() {
		end := event.MemoryEnd(l.agentID, name, callID, started, err)
		end.Tokens = cost.tokens
		end.Retries = cost.retries
		l.publish(ctx, end)
	}()

	tool, ok := l.tools[name]
	if !ok {
		return agenttool.Output{}, fmt.Errorf("tool %q not found", name)
	}
	out, err = tool.Execute(ctx, agenttool.Call{
		ID:        callID,
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return out, err
	}
	if name == compactMemoryToolName {
		if err := l.applyCompactTail(out); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (l *Loop) applyCompactTail(out agenttool.Output) error {
	if len(out.Details) == 0 {
		return nil
	}
	var tail []Message
	if err := json.Unmarshal(out.Details, &tail); err != nil {
		return nil
	}
	l.mu.Lock()
	l.messages = tail
	l.mu.Unlock()
	return l.persistReact()
}
