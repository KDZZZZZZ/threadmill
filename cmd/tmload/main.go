// Command tmload 在不调用模型的情况下对 Threadmill 基底做压测。
//
// 负载模型参数取自 DeepSWE 批次 all-03/all-04 的 67 个真实 agent 轨迹：
//   - 模型思考间隔：均值 ~8s（p50 6.1s / p90 14.6s）
//   - bash 命令时长：p50 35ms / p90 3.5s / p99 21s，均值 1.43s
//   - 每 agent 生命周期：fork 一次性环境 → 若干轮（读/grep/编辑/bash）→ absorb 释放
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/exec"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func main() {
	agents := flag.Int("agents", 16, "并行模拟 agent 数")
	turns := flag.Int("turns", 40, "每个 agent 的 ReAct 轮数")
	slots := flag.Int("slots", 8, "执行槽位数")
	files := flag.Int("files", 3000, "基线仓文件数")
	fileKB := flag.Int("file-kb", 4, "基线仓单文件大小")
	thinkMult := flag.Float64("think-mult", -1, "覆盖思考间隔缩放；负数按 command-duty 自动校准")
	commandDuty := flag.Float64("command-duty", 0.12, "目标命令服务时间/agent 生命周期")
	timeScale := flag.Float64("time-scale", 0.05, "等比压缩思考与 sleep 命令时长")
	liveRoot := flag.String("live-root", "", "VFS live 根目录；指向 reflink 文件系统时物化退化为块级克隆")
	seed := flag.Int64("seed", 42, "随机种子")
	repo := flag.String("repo", "", "基线仓目录；空则生成 fixture（-live-root 设定时 fixture 建在同盘以启用 reflink）")
	workdir := flag.String("workdir", "", "运行根目录；空则用临时目录")
	flag.Parse()

	if err := run(
		*agents, *turns, *slots, *files, *fileKB,
		*thinkMult, *commandDuty, *timeScale,
		*liveRoot, *seed, *repo, *workdir,
	); err != nil {
		fmt.Fprintln(os.Stderr, "tmload:", err)
		os.Exit(1)
	}
}

func run(
	agents, turns, slots, fileCount, fileKB int,
	thinkMult, commandDuty, timeScale float64,
	liveRoot string,
	seed int64,
	repo, workdir string,
) (retErr error) {
	if timeScale <= 0 {
		return fmt.Errorf("time-scale must be positive")
	}
	if thinkMult < 0 {
		if commandDuty <= 0 || commandDuty >= 1 {
			return fmt.Errorf("command-duty must be between zero and one")
		}
		thinkMult = targetThinkMultiplier(commandDuty, timeScale)
	}
	temp := workdir == ""
	if temp {
		dir, err := os.MkdirTemp("", "tmload-*")
		if err != nil {
			return err
		}
		workdir = dir
		defer os.RemoveAll(workdir)
	}
	fixtureRoot := workdir
	if liveRoot != "" {
		// fixture 与 live 同盘，Materialize 才能走 reflink（跨设备会退化为全量拷贝）。
		if err := os.MkdirAll(liveRoot, 0o750); err != nil {
			return err
		}
		fixtureRoot = filepath.Join(liveRoot, ".tmload-fixture")
		defer os.RemoveAll(fixtureRoot)
	}
	if repo == "" {
		dir, err := makeFixtureRepo(fixtureRoot, fileCount, fileKB)
		if err != nil {
			return err
		}
		repo = dir
	}
	if liveRoot == "" {
		liveRoot = filepath.Join(workdir, "live")
	}
	store, err := vfs.NewPersistentStoreWithOptions(
		repo,
		liveRoot,
		vfs.Options{Overlay: true},
	)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, store.Close()) }()
	// 压测默认关闭重车道：合成 sleep 会被时长画像误分类为重命令，
	// 干扰对基底开销的度量；车道效果由 heavylane_test 覆盖。
	sched := exec.New(exec.Config{
		Slots:           slots,
		Timeout:         120 * time.Second,
		ExternalSandbox: true,
		HeavyThreshold:  24 * time.Hour,
	})
	parent := "task-root"
	if err := seedParent(store, parent, fileCount); err != nil {
		return err
	}

	m := newMetrics()
	started := time.Now()
	var wg sync.WaitGroup
	for i := range agents {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			a := &simAgent{
				id:    id,
				envID: fmt.Sprintf("task-root:agent-%d", id),
				rng:   rand.New(rand.NewSource(seed + int64(id) + 1)),
				store: store,
				view:  store.View(fmt.Sprintf("task-root:agent-%d", id)),
				exec:  sched.View(fmt.Sprintf("task-root:agent-%d", id), store),
				files: fileCount,
				turns: turns,
				think: thinkMult,
				scale: timeScale,
				m:     m,
			}
			a.work(context.Background())
		}(i)
	}
	wg.Wait()
	wall := time.Since(started)

	report(
		agents, turns, wall, commandDuty, thinkMult, timeScale,
		store, sched, m, repo, liveRoot,
	)
	var cleanupErrors []error
	for i := range agents {
		if err := store.Discard(fmt.Sprintf("task-root:agent-%d", i)); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if err := store.Discard(parent); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

type simAgent struct {
	id    int
	envID string
	rng   *rand.Rand
	store *vfs.Store
	view  *vfs.View
	exec  env.ExecView
	files int
	turns int
	think float64
	scale float64
	m     *metrics
}

func (a *simAgent) work(ctx context.Context) {
	agentStarted := time.Now()
	defer func() { a.m.observe("agent", time.Since(agentStarted)) }()
	t0 := time.Now()
	if err := a.store.Fork("task-root", a.envID); err != nil {
		fmt.Fprintf(os.Stderr, "agent %d fork: %v\n", a.id, err)
		return
	}
	a.m.observe("fork", time.Since(t0))

	for turn := 0; turn < a.turns; turn++ {
		// 思考间隔：真实分布 p50=6s mean=8s，按 think 系数压缩。
		time.Sleep(time.Duration(sampleThink(a.rng) * float64(time.Second) * a.think))

		// 每轮 0-2 次文件读（真实：每模型调用 ~0.6 次 read）。
		for range a.rng.Intn(3) {
			path := fixturePath(a.rng, a.files)
			t1 := time.Now()
			if _, err := a.view.Read(path); err == nil {
				a.m.observe("read", time.Since(t1))
			}
		}
		// ~25% 一次目录列举（对应 grep/find 的进程内路径）。
		if a.rng.Float64() < 0.25 {
			t1 := time.Now()
			if _, err := a.view.List("."); err == nil {
				a.m.observe("list", time.Since(t1))
			}
		}
		// ~25% 一次小编辑（真实：edit 均值 ~0.4KB 改动，走 overlay 写）。
		if a.rng.Float64() < 0.25 {
			path := fixturePath(a.rng, a.files)
			t1 := time.Now()
			if err := a.view.Write(path, payload(a.rng)); err == nil {
				a.m.observe("write", time.Since(t1))
			}
		}
		// ~60% 一条 bash（真实：bash/model ≈ 0.6）。
		if a.rng.Float64() < 0.6 {
			command := sampleCommand(a.rng, a.scale)
			t1 := time.Now()
			if _, err := a.exec.Run(ctx, env.Cmd{Command: command, Timeout: 60 * time.Second}); err != nil {
				a.m.observe("bash_err", time.Since(t1))
				continue
			}
			a.m.observe("bash", time.Since(t1))
		}
	}

	t2 := time.Now()
	if err := a.store.Release(a.envID); err != nil {
		fmt.Fprintf(os.Stderr, "agent %d release: %v\n", a.id, err)
	}
	a.m.observe("release", time.Since(t2))
}

const (
	expectedThinkSeconds          = 7.73
	expectedCommandSecondsPerTurn = 1.308
)

func targetThinkMultiplier(commandDuty, timeScale float64) float64 {
	command := expectedCommandSecondsPerTurn * timeScale
	targetThink := command * (1 - commandDuty) / commandDuty
	return targetThink / expectedThinkSeconds
}

// sampleThink 按真实模型调用时长分布采样（秒）：50% 2-6s，35% 6-12s，13% 12-20s，2% 20-30s。
func sampleThink(rng *rand.Rand) float64 {
	switch r := rng.Float64(); {
	case r < 0.50:
		return 2 + rng.Float64()*4
	case r < 0.85:
		return 6 + rng.Float64()*6
	case r < 0.98:
		return 12 + rng.Float64()*8
	default:
		return 20 + rng.Float64()*10
	}
}

// sampleCommand 按真实 bash 时长分布采样，但使用固定命令串：
// 真实 agent 会原样重跑同一测试命令，参数化 sleep 会让命令指纹永不重复、
// 低估缓存命中率。时长靠固定档位的 sleep 表达。
func sampleCommand(rng *rand.Rand, timeScale float64) string {
	switch r := rng.Float64(); {
	case r < 0.50:
		switch rng.Intn(3) {
		case 0:
			return "true"
		case 1:
			return "ls . >/dev/null"
		default:
			return "find . -maxdepth 1 -type f | wc -l >/dev/null"
		}
	case r < 0.90:
		return scaledSleep(2, timeScale)
	case r < 0.99:
		return scaledSleep(12, timeScale)
	default:
		return scaledSleep(30, timeScale)
	}
}

func scaledSleep(seconds, scale float64) string {
	return fmt.Sprintf("sleep %.6f", seconds*scale)
}

func fixturePath(rng *rand.Rand, files int) string {
	idx := rng.Intn(files)
	// 目录归属必须与 makeFixtureRepo 的 idx%64 规则一致，否则读到不存在的路径。
	return fmt.Sprintf("src/pkg-%d/file_%d.txt", idx%64, idx)
}

func payload(rng *rand.Rand) []byte {
	size := 256 + rng.Intn(3584)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte('a' + rng.Intn(26))
	}
	return data
}

func makeFixtureRepo(root string, files, fileKB int) (string, error) {
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o750); err != nil {
		return "", err
	}
	chunk := make([]byte, 1024)
	for i := range chunk {
		chunk[i] = byte('0' + i%10)
	}
	data := make([]byte, 0, fileKB*1024)
	for range fileKB {
		data = append(data, chunk...)
	}
	for i := 0; i < files; i++ {
		dir := filepath.Join(repo, "src", fmt.Sprintf("pkg-%d", i%64))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file_%d.txt", i)), data, 0o640); err != nil {
			return "", err
		}
	}
	return repo, nil
}

func seedParent(store *vfs.Store, parent string, files int) error {
	view := store.View(parent)
	if err := view.Write("src/task_marker.txt", []byte("parent overlay seed")); err != nil {
		return err
	}
	if _, err := view.Read(fixturePath(rand.New(rand.NewSource(1)), files)); err != nil {
		// 基线文件读得到即可；读失败说明 fixture 有问题。
		return err
	}
	_, err := store.Materialize(parent)
	return err
}

type metrics struct {
	mu   sync.Mutex
	data map[string][]time.Duration
}

func newMetrics() *metrics {
	return &metrics{data: make(map[string][]time.Duration)}
}

func (m *metrics) observe(op string, d time.Duration) {
	m.mu.Lock()
	m.data[op] = append(m.data[op], d)
	m.mu.Unlock()
}

func (m *metrics) summary() map[string]opStat {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]opStat, len(m.data))
	for op, ds := range m.data {
		sorted := append([]time.Duration(nil), ds...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		out[op] = opStat{
			n:    len(sorted),
			p50:  pct(sorted, 50),
			p95:  pct(sorted, 95),
			max:  sorted[len(sorted)-1],
			mean: mean(sorted),
		}
	}
	return out
}

func (m *metrics) total(op string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total time.Duration
	for _, duration := range m.data[op] {
		total += duration
	}
	return total
}

type opStat struct {
	n    int
	p50  time.Duration
	p95  time.Duration
	max  time.Duration
	mean time.Duration
}

func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)*p/100, len(sorted)-1)]
}

func mean(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range ds {
		total += d
	}
	return total / time.Duration(len(ds))
}

func report(
	agents, turns int,
	wall time.Duration,
	commandDuty, thinkMultiplier, timeScale float64,
	store *vfs.Store,
	sched *exec.Scheduler,
	m *metrics,
	repo, liveRoot string,
) {
	vs := store.Stats()
	es := sched.Stats()
	agentTime := m.total("agent")
	realizedDuty := 0.0
	if agentTime > 0 {
		realizedDuty = float64(es.RunDuration) / float64(agentTime)
	}
	fmt.Printf("tmload: agents=%d turns=%d wall=%s\n", agents, turns, wall.Truncate(time.Millisecond))
	fmt.Printf("load: command_duty_target=%.3f command_duty_realized=%.3f command_service=%s agent_lifecycle=%s think_multiplier=%.4f time_scale=%.4f\n",
		commandDuty, realizedDuty, es.RunDuration.Truncate(time.Millisecond),
		agentTime.Truncate(time.Millisecond), thinkMultiplier, timeScale)
	fmt.Printf("vfs: envs=%d live=%d overlay_files=%d overlay_MB=%.1f materialize=%d [overlay=%d reflink=%d full_copy=%d fallback=%d(capacity=%d error=%d)] materialize_time=%s absorb_fast=%d upper=%d entries=%d fallbacks=%d errors=%d(%s) absorb_scans=%d(%s) absorb_errors=%d absorb_peak=%d/%d absorb_wait=%s\n",
		vs.Environments, vs.LiveDirs, vs.OverlayFiles, float64(vs.OverlayBytes)/1e6,
		vs.MaterializeCopies, vs.MaterializeOverlays, vs.MaterializeReflinks,
		vs.MaterializeFullCopies, vs.MaterializeFallbacks,
		vs.OverlayCapacityFallbacks, vs.OverlayErrorFallbacks,
		vs.MaterializeCopyDuration.Truncate(time.Millisecond),
		vs.AbsorbFastPaths,
		vs.AbsorbUpperAttempts, vs.AbsorbUpperEntries, vs.AbsorbUpperFallbacks,
		vs.AbsorbUpperErrors, vs.AbsorbUpperDuration.Truncate(time.Millisecond),
		vs.AbsorbScans, vs.AbsorbScanDuration.Truncate(time.Millisecond), vs.AbsorbScanErrors,
		vs.AbsorbPeakActive, vs.AbsorbCapacity, vs.AbsorbWaitDuration.Truncate(time.Millisecond))
	fmt.Printf("vfs: live_root=%s overlay_backend=%s overlay_active=%d/%d reflink=%v\n",
		liveRoot, vs.OverlayBackend, vs.OverlayActive, vs.OverlayCapacity,
		vfs.ReflinkCloneable(repo, liveRoot))
	if vs.OverlayLastFallback != "" {
		fmt.Printf("vfs: overlay_last_fallback=%q\n", vs.OverlayLastFallback)
	}
	fmt.Printf("exec: backend=%s capacity=%d requests=%d queued_peak=%d active_peak=%d wait_total=%s run_total=%s\n",
		es.SandboxBackend, es.Capacity, es.Requests, es.PeakQueued, es.PeakActive,
		es.WaitDuration.Truncate(time.Millisecond), es.RunDuration.Truncate(time.Millisecond))
	fmt.Printf("rss_peak=%s disk_live=%s\n", peakRSS(), dirSize(liveRoot))
	fmt.Println("op          n      p50       p95       max        mean")
	for _, op := range []string{"fork", "read", "list", "write", "bash", "bash_err", "release"} {
		s, ok := m.summary()[op]
		if !ok {
			continue
		}
		fmt.Printf("%-10s %5d  %8s  %8s  %9s  %8s\n", op, s.n,
			s.p50.Truncate(time.Microsecond), s.p95.Truncate(time.Millisecond),
			s.max.Truncate(time.Millisecond), s.mean.Truncate(time.Microsecond))
	}
}

func peakRSS() string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "?"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:"))
		}
	}
	return "?"
}

func dirSize(dir string) string {
	var total int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, statErr := d.Info(); statErr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return fmt.Sprintf("%.1fMB", float64(total)/1e6)
}
