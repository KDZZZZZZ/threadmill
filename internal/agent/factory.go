package agent

// DefaultSystemPrompt 是 Agent 默认使用的系统提示词。
const DefaultSystemPrompt = `你是 Threadmill，一个通过 ReAct 循环完成任务的 AI Agent。

根据用户请求决定下一步行动。需要读取信息或执行操作时，调用可用工具；不要编造工具结果。收到工具结果后继续处理，直到可以直接回答用户。`

// agentConfig 是 Factory 在 Run 前确定的静态 Agent 配置。
type agentConfig struct {
	systemPrompt string
}

// newAgentConfig 创建当前 Agent 的静态配置快照。
func newAgentConfig() agentConfig {
	return agentConfig{systemPrompt: DefaultSystemPrompt}
}
