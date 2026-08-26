package exec

import (
	"context"
	"os"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/cmdcache"
	"github.com/KDZZZZZZ/threadmill/internal/env"
)

// maxTraceBytes 是单次执行允许消费的追踪字节数。
// 超出即把观测判为不可信，本次结果不入缓存——巨型构建不值得为它保留无界状态。
const maxTraceBytes = 64 << 20

func (s *Scheduler) cacheEnabled() bool {
	return s != nil && s.cache != nil && s.tracing
}

func (s *Scheduler) cacheKey(command string) cmdcache.Key {
	backend, _ := s.isolationBoundary()
	return cmdcache.Key{
		Command: command,
		Backend: backend,
		EnvHash: cacheEnvHash(backend),
	}
}

// lookupCache 找一条依赖在当前 live 树里仍然成立的条目。
// 查找失败绝不能让命令跑不起来，所以出错一律当 miss。
func (s *Scheduler) lookupCache(live string, key cmdcache.Key) *cmdcache.Entry {
	if !s.cacheEnabled() {
		return nil
	}
	entry, err := s.cache.Lookup(live, key)
	if err != nil {
		return nil
	}
	return entry
}

func cachedResult(entry *cmdcache.Entry) env.ExecResult {
	result := entry.Result()
	// PeakRSSBytes 留零：命中没有真的跑进程，报一个历史值会污染容量规划。
	return env.ExecResult{ExitCode: result.ExitCode, Output: result.Output}
}

// storeTrace 把一次真实执行的结果连同推断出的依赖写进缓存。
//
// 只在结果确实代表命令本身时才存：沙箱错误、超时、取消反映的是环境而不是
// 命令，存下来会把一次偶发故障固化成所有 agent 的既定结论。
func (s *Scheduler) storeTrace(
	ctx context.Context,
	live string,
	key cmdcache.Key,
	trace *traceRun,
	result env.ExecResult,
	runErr error,
	took time.Duration,
) *cmdcache.Entry {
	defer trace.discard()
	if !s.cacheEnabled() || trace == nil || runErr != nil || ctx.Err() != nil {
		return nil
	}
	obs, ok := trace.observe()
	if !ok {
		return nil
	}
	entry, err := s.cache.Store(live, key, obs, cmdcache.Result{
		ExitCode: result.ExitCode,
		Output:   result.Output,
		Duration: took,
	})
	if err != nil {
		return nil
	}
	return entry
}

// observe 解析追踪文件。读不出来就当作没有观测，本次结果不入缓存。
func (t *traceRun) observe() (cmdcache.Observation, bool) {
	if t == nil {
		return cmdcache.Observation{}, false
	}
	file, err := os.Open(t.hostOutput)
	if err != nil {
		return cmdcache.Observation{}, false
	}
	defer file.Close()
	obs, err := cmdcache.ParseTrace(file, t.root, t.tmp, maxTraceBytes)
	if err != nil {
		return cmdcache.Observation{}, false
	}
	return obs, true
}

// reconcileVerification 对账一次抽样验证：命中后仍然照常执行，比对真实结果
// 与缓存条目。
//
// 读集是观测得来的，不是证明出来的——某次执行没走到的分支，它的依赖就不在
// 集合里，之后会静默误命中。抽样对账是发现这类问题的唯一手段。
//
// 不一致说明这条命令在同样的依赖状态下会给出不同结果：要么读集漏了依赖，
// 要么命令本身不确定。两种情况都不该继续缓存，所以连刚存进去的新条目一起删掉
// （新旧条目的依赖状态相同，ID 也相同，删一次即可）。
func (s *Scheduler) reconcileVerification(key cmdcache.Key, hit, fresh *cmdcache.Entry) {
	if hit == nil {
		return
	}
	if fresh != nil && sameResult(hit, fresh) {
		return
	}
	_ = s.cache.Invalidate(key, hit)
}

func sameResult(a, b *cmdcache.Entry) bool {
	if a.ExitCode != b.ExitCode || a.Output != b.Output || len(a.Writes) != len(b.Writes) {
		return false
	}
	produced := make(map[string]cmdcache.Change, len(a.Writes))
	for _, change := range a.Writes {
		produced[change.Path] = change
	}
	for _, change := range b.Writes {
		previous, ok := produced[change.Path]
		if !ok || previous.Kind != change.Kind ||
			previous.Digest != change.Digest ||
			previous.Target != change.Target ||
			previous.Executable != change.Executable {
			return false
		}
	}
	return true
}
