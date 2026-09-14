package exec

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxCostEntries = 4096

// procMeter 以固定间隔轮询 /proc，累计一棵命令进程树的常驻内存峰值。
// 无权限要求、跨沙箱形态可用；cgroup v2 精确计量在 Scheduler v2 的字节闸中接入。
type procMeter struct {
	interval time.Duration
	pagesize int64
}

func newProcMeter() procMeter {
	return procMeter{interval: 50 * time.Millisecond, pagesize: int64(os.Getpagesize())}
}

// sampleOnce 返回 rootPID 及其当前后代的 RSS 总和（字节）。
func (m procMeter) sampleOnce(rootPID int) int64 {
	pending := []string{strconv.Itoa(rootPID)}
	seen := make(map[string]struct{})
	var total int64
	for len(pending) > 0 {
		last := len(pending) - 1
		pid := pending[last]
		pending = pending[:last]
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		total += m.procRSSKB(pid) * 1024
		pending = append(pending, procChildren(pid)...)
	}
	return total
}

// procChildren 返回一个进程所有线程当前的一层子进程 PID。
func procChildren(pid string) []string {
	tasks, err := os.ReadDir("/proc/" + pid + "/task")
	if err != nil {
		return nil
	}
	var children []string
	for _, task := range tasks {
		if !task.IsDir() {
			continue
		}
		data, err := os.ReadFile("/proc/" + pid + "/task/" + task.Name() + "/children")
		if err != nil {
			continue
		}
		for _, child := range strings.Fields(string(data)) {
			if _, err := strconv.Atoi(child); err == nil {
				children = append(children, child)
			}
		}
	}
	return children
}

// procRSSKB 读取 /proc/<pid>/statm 的第二字段（共享页计数的 resident 页数）。
func (m procMeter) procRSSKB(pid string) int64 {
	statm, err := os.ReadFile("/proc/" + pid + "/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(statm))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * m.pagesize / 1024
}

// run 采样直到 stop 关闭，返回观测到的 RSS 峰值（字节）。
func (m procMeter) run(rootPID int, stop <-chan struct{}) int64 {
	var peak int64
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return peak
		case <-ticker.C:
			if current := m.sampleOnce(rootPID); current > peak {
				peak = current
			}
		}
	}
}

// CostStat 是单个命令指纹的累计开销画像，供 Scheduler v2 做车道分类与内存记账准入。
type CostStat struct {
	Count    uint64
	Duration time.Duration
	PeakRSS  uint64 // 观测到的最大 RSS 峰值（字节）
}

// CostSnapshot 是命令指纹到累计开销的并发一致副本。
type CostSnapshot map[string]CostStat

// costTable 按 command 汇总时长与峰值 RSS。
type costTable struct {
	mu    sync.Mutex
	stats map[string]*CostStat
	order [maxCostEntries]string
	next  int
}

func newCostTable() *costTable {
	return &costTable{stats: make(map[string]*CostStat)}
}

func (t *costTable) record(command string, took time.Duration, peakRSS uint64) {
	if command == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	stat := t.stats[command]
	if stat == nil {
		if len(t.stats) == maxCostEntries {
			delete(t.stats, t.order[t.next])
			t.order[t.next] = command
			t.next = (t.next + 1) % maxCostEntries
		} else {
			t.order[len(t.stats)] = command
		}
		stat = &CostStat{}
		t.stats[command] = stat
	}
	stat.Count++
	stat.Duration += took
	if peakRSS > stat.PeakRSS {
		stat.PeakRSS = peakRSS
	}
}

func (t *costTable) lookup(command string) (CostStat, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	stat, ok := t.stats[command]
	if !ok {
		return CostStat{}, false
	}
	return *stat, true
}

func (t *costTable) snapshot() CostSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(CostSnapshot, len(t.stats))
	for command, stat := range t.stats {
		out[command] = *stat
	}
	return out
}
