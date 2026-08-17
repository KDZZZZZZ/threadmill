package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

const bashName = "bash"

type bashTool struct {
	exec env.ExecView
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
	return t
}

func (t bashTool) Definition() Definition {
	return Definition{
		Name:        bashName,
		Description: "在工作区里执行 bash 命令。可选 timeout 是秒数。非零退出码会写在输出里，不是工具错误。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"要执行的 bash 命令"},"timeout":{"type":"integer","description":"超时秒数"}},"required":["command"],"additionalProperties":false}`),
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
	}
	if err := decodeMemoryArgs(call.Arguments, &args); err != nil {
		return Output{}, err
	}
	if args.Command == "" {
		return Output{}, fmt.Errorf("%s: missing command", bashName)
	}
	spec := env.Cmd{Command: args.Command}
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
	return Output{Content: content}, nil
}
