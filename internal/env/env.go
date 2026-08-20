// Package env 是一个 task 的隔离工作区：记忆、文件、执行视图挂在同一个 ID 上。
package env

import (
	"context"
	"time"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

// MemoryView 是某个环境的记忆图视图。
type MemoryView interface {
	Snapshot() ctxgraph.Graph
	Commit(ctxgraph.Graph) error
}

// FileInfo 是路径上的文件元数据。
type FileInfo struct {
	Name  string
	Size  int64
	IsDir bool
}

// DirEnt 是目录里的一项。
type DirEnt struct {
	Name  string
	IsDir bool
}

// FileView 是某个环境的工作区文件视图。
type FileView interface {
	Read(path string) ([]byte, error)
	Write(path string, data []byte) error
	Delete(path string) error
	Stat(path string) (FileInfo, error)
	List(path string) ([]DirEnt, error)
}

// Cmd 是一次要在环境里执行的命令。
type Cmd struct {
	Command string
	Timeout time.Duration
}

// ExecResult 是命令跑完后的合流输出。
type ExecResult struct {
	ExitCode int
	Output   string
}

// ExecView 是某个环境的命令执行视图。
type ExecView interface {
	Run(ctx context.Context, spec Cmd) (ExecResult, error)
}

// Env 是一个 task 的隔离工作区。同一 task 的角色共用一份。
type Env struct {
	ID     string
	Memory MemoryView
	Files  FileView
	Exec   ExecView
}

// Open 按环境 ID 装上记忆视图。Files 和 Exec 为零值 nil。
func Open(id string, memory MemoryView) Env {
	return Env{ID: id, Memory: memory}
}

// WithFiles 挂上文件视图，不改 ID 和 Memory。
func (e Env) WithFiles(files FileView) Env {
	e.Files = files
	return e
}

// WithExec 挂上命令执行视图，不改 ID、Memory 和 Files。
func (e Env) WithExec(x ExecView) Env {
	e.Exec = x
	return e
}
