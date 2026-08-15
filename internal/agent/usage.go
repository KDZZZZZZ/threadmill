package agent

// Usage 保存一次 Provider 请求返回的原始 Token 用量。
type Usage struct {
	InputTokens      int
	CachedTokens     int
	CacheWriteTokens int
	OutputTokens     int
	ReasoningTokens  int
	TotalTokens      int
}

// ContextTokens 返回本次请求占用的上下文 Token 数。
func (u Usage) ContextTokens() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.InputTokens + u.CachedTokens + u.CacheWriteTokens + u.OutputTokens + u.ReasoningTokens
}

// ShouldCompact 在已配置窗口且用量超过窗口时触发压缩。
func ShouldCompact(usage *Usage, contextWindow int) bool {
	if usage == nil || contextWindow <= 0 {
		return false
	}
	return usage.ContextTokens() > contextWindow
}

// cloneUsage 复制可选用量，避免 Hook 或调用方修改内部历史。
func cloneUsage(usage *Usage) *Usage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}
