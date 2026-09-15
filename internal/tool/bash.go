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
		Description: "在工作区根目录执行 bash。工作区内文件必须使用相对路径；系统工具和外部依赖可用绝对路径。不执行 git commit 或 git push；保留文件修改，由 Threadmill 保存快照；真实目录 task 的改动直接可见。普通调用结束时，它启动的所有后代进程随之终止，& 或 nohup 不能让进程脱离本次调用。需要跨调用存活的服务（dev server、watch 构建）设 run_in_background=true 显式声明：立即返回 id，用 bash_output 读输出、bash_kill 终止，当前角色结束时自动终止。可选 timeout 是秒数，只作用于前台调用。非零退出码会写在输出里，不是工具错误。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"要执行的 bash 命令"},"timeout":{"type":"integer","description":"超时秒数，只作用于前台调用"},"run_in_background":{"type":"boolean","description":"显式声明为后台命令：立即返回 id，不等待结束"}},"required":["command"],"additionalProperties":false}`),
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
		Command         string `json:"command"`
		Timeout         *int   `json:"timeout"`
		RunInBackground bool   `json:"run_in_background"`
	}
	if err := decodeMemoryArgs(call.Arguments, &args); err != nil {
		return Output{}, err
	}
	if args.Command == "" {
		return Output{}, fmt.Errorf("%s: missing command", bashName)
	}
	if args.RunInBackground {
		bg, err := backgroundExec(t.exec, bashName)
		if err != nil {
			return Output{}, err
		}
		st, err := bg.Start(ctx, env.Cmd{Command: args.Command})
		if err != nil {
			return Output{}, err
		}
		return Output{Content: fmt.Sprintf(
			"已在后台启动 %s。用 bash_output 读取输出和状态，用 bash_kill 终止；当前角色结束时自动终止。\n%s",
			st.ID, formatBackground(st),
		)}, nil
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
