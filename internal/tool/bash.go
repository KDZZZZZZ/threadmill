package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

const bashName = "bash"

type bashTool struct {
	exec env.ExecView
	role string
}

var (
	_ Tool      = bashTool{}
	_ EnvBinder = bashTool{}
)

// Bash 返回在 Env.Exec 里跑命令的工具。未 BindEnv 时 Execute 报基础设施错误。
func Bash() Tool {
	return bashTool{}
}

func (t bashTool) BindEnv(e env.Env) Tool {
	t.exec = e.Exec
	t.role = e.ID[strings.LastIndex(e.ID, ":")+1:]
	switch t.role {
	case "manager", "planner", "executor", "verifier", "input":
	default:
		t.role = "unknown"
	}
	return t
}

func (t bashTool) Definition() Definition {
	return Definition{
		Name:        bashName,
		Description: "在工作区根目录执行 bash。工作区内文件必须使用相对路径；系统工具和外部依赖可用绝对路径。不执行 git commit 或 git push；保留文件修改，由 Threadmill 保存快照；真实目录 task 的改动直接可见。仍存活的后代进程由环境结束时回收，其执行结果不入缓存。可选 timeout 是秒数；fresh=true 绕过缓存重新执行。命中会显示 [cached: …]。非零退出码会写在输出里，不是工具错误。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"要执行的 bash 命令"},"timeout":{"type":"integer","description":"超时秒数"},"fresh":{"type":"boolean","description":"跳过缓存，重新执行命令"}},"required":["command"],"additionalProperties":false}`),
	}
}

func (t bashTool) Execute(ctx context.Context, call Call) (Output, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	if t.exec == nil {
		return Output{}, fmt.Errorf("%s: not bound to env", bashName)
	}
	var args struct {
		Command string `json:"command"`
		Timeout *int   `json:"timeout"`
		Fresh   bool   `json:"fresh"`
	}
	if err := decodeMemoryArgs(call.Arguments, &args); err != nil {
		return Output{}, err
	}
	if args.Command == "" {
		return Output{}, fmt.Errorf("%s: missing command", bashName)
	}
	spec := env.Cmd{Command: args.Command, Fresh: args.Fresh, Role: t.role}
	if args.Timeout != nil && *args.Timeout > 0 {
		spec.Timeout = time.Duration(*args.Timeout) * time.Second
	}
	res, err := t.exec.Run(ctx, spec)
	if err != nil {
		return Output{}, err
	}
	content := res.Output
	if res.ExitCode != 0 {
		content = fmt.Sprintf("exit %d\n%s", res.ExitCode, res.Output)
	}
	if res.CachedSegments > 0 {
		content = fmt.Sprintf("[cached: %d segments, saved %s, creator %s]\n%s",
			res.CachedSegments, res.CacheSavedDuration, res.CacheCreatorRole, content)
	}
	return Output{Content: content}, nil
}
