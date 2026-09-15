package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

const (
	// BashOutputName 读取后台命令的增量输出。
	BashOutputName = "bash_output"
	// BashKillName 终止后台命令。
	BashKillName = "bash_kill"

	maxBackgroundWaitSeconds = 600
)

// ShellTools 返回 bash 及其后台命令伴随工具。
func ShellTools() []Tool {
	return []Tool{Bash(), BashOutput(), BashKill()}
}

type bashOutputTool struct {
	exec env.ExecView
}

var (
	_ Tool      = bashOutputTool{}
	_ EnvBinder = bashOutputTool{}
)

// BashOutput 返回读取 run_in_background 命令输出与状态的工具。
func BashOutput() Tool {
	return bashOutputTool{}
}

func (t bashOutputTool) BindEnv(e env.Env) Tool {
	t.exec = e.Exec
	return t
}

func (t bashOutputTool) Definition() Definition {
	return Definition{
		Name:        BashOutputName,
		Description: "读取 bash run_in_background 启动的后台命令自上次读取以来的新输出和状态（running、exited N 或 killed）。可选 wait 秒数：最多等待这么久，命令退出即提前返回，用来代替频繁轮询。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"bash run_in_background 返回的后台命令 id"},"wait":{"type":"integer","minimum":0,"maximum":600,"description":"最多等待的秒数"}},"required":["id"],"additionalProperties":false}`),
	}
}

func (t bashOutputTool) Execute(ctx context.Context, call Call) (Output, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	bg, err := backgroundExec(t.exec, BashOutputName)
	if err != nil {
		return Output{}, err
	}
	var args struct {
		ID   string `json:"id"`
		Wait *int   `json:"wait"`
	}
	if err := decodeMemoryArgs(call.Arguments, &args); err != nil {
		return Output{}, err
	}
	if args.ID == "" {
		return Output{}, fmt.Errorf("%s: missing id", BashOutputName)
	}
	var wait time.Duration
	if args.Wait != nil && *args.Wait > 0 {
		wait = time.Duration(min(*args.Wait, maxBackgroundWaitSeconds)) * time.Second
	}
	st, err := bg.Output(ctx, args.ID, wait)
	if err != nil {
		return Output{}, err
	}
	return Output{Content: formatBackground(st)}, nil
}

type bashKillTool struct {
	exec env.ExecView
}

var (
	_ Tool      = bashKillTool{}
	_ EnvBinder = bashKillTool{}
)

// BashKill 返回终止 run_in_background 命令的工具。
func BashKill() Tool {
	return bashKillTool{}
}

func (t bashKillTool) BindEnv(e env.Env) Tool {
	t.exec = e.Exec
	return t
}

func (t bashKillTool) Definition() Definition {
	return Definition{
		Name:        BashKillName,
		Description: "终止 bash run_in_background 启动的后台命令及其全部后代，返回最后的输出和状态。服务用完即终止；角色结束时未终止的后台命令也会被强制回收。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"bash run_in_background 返回的后台命令 id"}},"required":["id"],"additionalProperties":false}`),
	}
}

func (t bashKillTool) Execute(ctx context.Context, call Call) (Output, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	bg, err := backgroundExec(t.exec, BashKillName)
	if err != nil {
		return Output{}, err
	}
	var args struct {
		ID string `json:"id"`
	}
	if err := decodeMemoryArgs(call.Arguments, &args); err != nil {
		return Output{}, err
	}
	if args.ID == "" {
		return Output{}, fmt.Errorf("%s: missing id", BashKillName)
	}
	st, err := bg.Kill(args.ID)
	if err != nil {
		return Output{}, err
	}
	return Output{Content: formatBackground(st)}, nil
}

func backgroundExec(view env.ExecView, name string) (env.BackgroundExec, error) {
	if view == nil {
		return nil, fmt.Errorf("%s: not bound to env", name)
	}
	bg, ok := view.(env.BackgroundExec)
	if !ok {
		return nil, fmt.Errorf("%s: background commands are unavailable in this environment", name)
	}
	return bg, nil
}

func formatBackground(st env.BackgroundStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\n", st.ID)
	switch {
	case st.Running:
		b.WriteString("status: running\n")
	case st.Killed:
		b.WriteString("status: killed\n")
	default:
		fmt.Fprintf(&b, "status: exited %d\n", st.ExitCode)
	}
	if st.Dropped > 0 {
		fmt.Fprintf(&b, "[%d bytes dropped before this read]\n", st.Dropped)
	}
	if st.Output == "" {
		b.WriteString("(no new output)")
	} else {
		b.WriteString(st.Output)
	}
	return b.String()
}
