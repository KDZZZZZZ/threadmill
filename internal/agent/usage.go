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

// ShouldCompact 在已配置窗口接近耗尽时触发压缩，为下一次模型输出留出空间。
func ShouldCompact(usage *Usage, contextWindow int) bool {
	if usage == nil || contextWindow <= 0 {
		return false
	}
	return usage.TotalTokens >= max(1, softContextThreshold(contextWindow))
}

// cloneUsage 复制可选用量，避免 Hook 或调用方修改内部历史。
func cloneUsage(usage *Usage) *Usage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}
