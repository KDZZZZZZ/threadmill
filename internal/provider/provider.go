package provider

import "net/http"

// NewFromRoot 读取根目录配置并创建 Responses Provider。
// root 必须可信：配置会决定读取哪个环境变量，以及将其发送到哪个端点。
func NewFromRoot(root string, client *http.Client) (*Responses, error) {
	config, err := LoadConfig(root)
	if err != nil {
		return nil, err
	}
	return NewResponses(config.LLM, client)
}
