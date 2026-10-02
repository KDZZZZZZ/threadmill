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

func (s *Scheduler) cacheKey(command, role string) cmdcache.Key {
	backend, _ := s.isolationBoundary()
	return cmdcache.Key{
		Command:      command,
		Backend:      backend,
		EnvHash:      cacheEnvHash(backend, s.outputCap),
		Role:         role,
		ExternalPath: s.externalDependencyPath,
		ExternalType: s.externalDependencyType,
	}
}

// lookupCache 找一条依赖在当前 live 树里仍然成立的条目。
// 查找失败绝不能让命令跑不起来，所以出错一律当 miss。
func (s *Scheduler) lookupCache(live, home string, key cmdcache.Key) *cmdcache.Entry {
	if !s.cacheEnabled() {
		return nil
	}
	entry, err := s.cache.Lookup(live, key, home)
	if err != nil {
		return nil
	}
	return entry
}

func cachedResult(entry *cmdcache.Entry) env.ExecResult {
	result := entry.Result()
	// PeakRSSBytes 留零：命中没有真的跑进程，报一个历史值会污染容量规划。
	return env.ExecResult{
		ExitCode: result.ExitCode, Output: result.Output,
		CachedSegments: 1, CacheSavedDuration: result.Duration, CacheCreatorRole: entry.CreatorRole,
	}
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
	if !s.cacheEnabled() {
		return nil
	}
	if runErr != nil || ctx.Err() != nil {
		s.cache.Reject("run_error")
		return nil
	}
	if trace == nil {
		s.cache.Reject("trace_unavailable")
		return nil
	}
	obs, ok := trace.observe()
	if !ok {
		if trace.incomplete {
			s.cache.Reject("trace_incomplete")
		} else {
			s.cache.Reject("trace_unavailable")
		}
		return nil
	}
	entry, err := s.cache.Store(live, key, obs, cmdcache.Result{
		ExitCode: result.ExitCode,
		Output:   result.Output,
		Duration: took,
	}, trace.hostHome)
	if err != nil {
		return nil
	}
	return entry
}

// observe 解析追踪文件。读不出来就当作没有观测，本次结果不入缓存。
func (t *traceRun) observe() (cmdcache.Observation, bool) {
	if t == nil || t.incomplete {
		return cmdcache.Observation{}, false
	}
	file, err := os.Open(t.hostOutput)
	if err != nil {
		return cmdcache.Observation{}, false
	}
	defer file.Close()
	obs, err := cmdcache.ParseTraceWithHome(file, t.root, t.tmp, t.home, maxTraceBytes)
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
// 要么命令本身不确定。两种情况都不该继续复用这个候选。新旧条目的依赖状态
// 相同时，ID 也相同；删掉候选会同时删掉刚存进去的不确定结果。
func (s *Scheduler) reconcileVerification(key cmdcache.Key, hit, fresh *cmdcache.Entry) {
	if hit == nil {
		return
	}
	kind := verificationMismatch(hit, fresh)
	s.cache.RecordVerification(kind)
	if kind == "" {
		return
	}
	_ = s.cache.Invalidate(key, hit)
}

func verificationMismatch(a, b *cmdcache.Entry) string {
	if b == nil {
		return "unavailable"
	}
	if a.ExitCode != b.ExitCode {
		return "exit_code"
	}
	if len(a.Writes) != len(b.Writes) {
		return "writes"
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
			return "writes"
		}
	}
	if a.Output != b.Output {
		return "output"
	}
	return ""
}
