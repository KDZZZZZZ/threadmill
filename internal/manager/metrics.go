package manager

import (
	"time"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	tmexec "github.com/KDZZZZZZ/threadmill/internal/exec"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

// TaskMetrics 汇总协调图中的终态分布。
type TaskMetrics struct {
	Total    int `json:"total"`
	Active   int `json:"active"`
	Done     int `json:"done"`
	Failed   int `json:"failed"`
	Canceled int `json:"canceled"`
}

// RuntimeMetrics 是每次采样时的 Go runtime 状态。
type RuntimeMetrics struct {
	Goroutines   int           `json:"goroutines"`
	HeapAlloc    uint64        `json:"heap_alloc"`
	HeapObjects  uint64        `json:"heap_objects"`
	GCCount      uint32        `json:"gc_count"`
	GCPauseTotal time.Duration `json:"gc_pause_total"`
}

// Metrics 是一次可用于压力测试和运行日志的监控快照。
type Metrics struct {
	Time        time.Time             `json:"time"`
	Uptime      time.Duration         `json:"uptime"`
	Pending     int                   `json:"pending"`
	TaskRunning bool                  `json:"task_running"`
	Tasks       TaskMetrics           `json:"tasks"`
	Events      event.MetricsSnapshot `json:"events"`
	Exec        tmexec.Stats          `json:"exec"`
	VFS         vfs.Stats             `json:"vfs"`
	Memory      ctxgraph.StoreStats   `json:"memory"`
	Runtime     RuntimeMetrics        `json:"runtime"`
}
