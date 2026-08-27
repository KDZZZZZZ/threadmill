package event

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"
)

var durationBounds = [...]time.Duration{
	10 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
	time.Minute,
}

// DurationBucket 是累计时延桶。UpperBound 为 0 的末桶表示 +Inf。
type DurationBucket struct {
	UpperBound time.Duration `json:"upper_bound"`
	Count      uint64        `json:"count"`
}

// DurationMetrics 是有界直方图及精确的样本数、总和、最大值。
type DurationMetrics struct {
	Count   uint64           `json:"count"`
	Total   time.Duration    `json:"total"`
	Max     time.Duration    `json:"max"`
	P50     time.Duration    `json:"p50"`
	P95     time.Duration    `json:"p95"`
	Buckets []DurationBucket `json:"buckets"`
}

// OperationMetrics 聚合一种有界生命周期事件。
type OperationMetrics struct {
	Started   uint64          `json:"started"`
	Completed uint64          `json:"completed"`
	Errors    uint64          `json:"errors"`
	Active    uint64          `json:"active"`
	Duration  DurationMetrics `json:"duration"`
	TTFT      DurationMetrics `json:"ttft,omitempty"`
}

// MetricsSnapshot 是事件采集器的一致快照；不包含 prompt、delta 正文或动态标签。
type MetricsSnapshot struct {
	Model                     OperationMetrics `json:"model"`
	Tool                      OperationMetrics `json:"tool"`
	Task                      OperationMetrics `json:"task"`
	Memory                    OperationMetrics `json:"memory"`
	Tokens                    uint64           `json:"tokens"`
	InputTokens               uint64           `json:"input_tokens"`
	CachedTokens              uint64           `json:"cached_tokens"`
	CacheWriteTokens          uint64           `json:"cache_write_tokens"`
	CacheHitRate              float64          `json:"cache_hit_rate"` // KindModel 请求
	ToolCalls                 uint64           `json:"tool_calls"`
	ModelRetries              uint64           `json:"model_retries"`
	MemoryTokens              uint64           `json:"memory_tokens"`
	MemoryInputTokens         uint64           `json:"memory_input_tokens"`
	MemoryCachedTokens        uint64           `json:"memory_cached_tokens"`
	MemoryCacheWriteTokens    uint64           `json:"memory_cache_write_tokens"`
	TotalCacheHitRate         float64          `json:"total_cache_hit_rate"`
	MemoryRetries             uint64           `json:"memory_retries"`
	MemoryOrganizerRuns       uint64           `json:"memory_organizer_runs"`
	MemoryOrganizerCandidates uint64           `json:"memory_organizer_candidates"`
	MemoryOrganizerSelected   uint64           `json:"memory_organizer_selected"`
	MemoryOrganizerTokens     uint64           `json:"memory_organizer_tokens"`
	MemoryOrganizerDuration   DurationMetrics  `json:"memory_organizer_duration"`
	StreamChunks              uint64           `json:"stream_chunks"`
	ModelStreamIdle           time.Duration    `json:"model_stream_idle"`
	MemoryStreamChunks        uint64           `json:"memory_stream_chunks"`
	MemoryStreamIdle          time.Duration    `json:"memory_stream_idle"`
	DeltaChunks               uint64           `json:"delta_chunks"`
	DeltaBytes                uint64           `json:"delta_bytes"`
}

type histogram struct {
	count uint64
	total time.Duration
	max   time.Duration
	bins  [len(durationBounds) + 1]uint64
}

func (h *histogram) observe(value time.Duration) {
	if value < 0 {
		return
	}
	h.count++
	h.total += value
	if value > h.max {
		h.max = value
	}
	for i, bound := range durationBounds {
		if value <= bound {
			h.bins[i]++
			return
		}
	}
	h.bins[len(durationBounds)]++
}

func (h histogram) snapshot() DurationMetrics {
	buckets := make([]DurationBucket, 0, len(h.bins))
	var cumulative uint64
	for i, count := range h.bins {
		cumulative += count
		bound := time.Duration(0)
		if i < len(durationBounds) {
			bound = durationBounds[i]
		}
		buckets = append(buckets, DurationBucket{UpperBound: bound, Count: cumulative})
	}
	return DurationMetrics{
		Count:   h.count,
		Total:   h.total,
		Max:     h.max,
		P50:     h.quantile(0.50),
		P95:     h.quantile(0.95),
		Buckets: buckets,
	}
}

func (h histogram) quantile(q float64) time.Duration {
	if h.count == 0 {
		return 0
	}
	rank := uint64(math.Ceil(q * float64(h.count)))
	var cumulative uint64
	for i, count := range h.bins {
		cumulative += count
		if cumulative < rank {
			continue
		}
		if i < len(durationBounds) {
			if durationBounds[i] < h.max {
				return durationBounds[i]
			}
			return h.max
		}
		return h.max
	}
	return h.max
}

type operation struct {
	started   uint64
	completed uint64
	errors    uint64
	active    uint64
	duration  histogram
	ttft      histogram
}

func (o operation) snapshot() OperationMetrics {
	return OperationMetrics{
		Started:   o.started,
		Completed: o.completed,
		Errors:    o.errors,
		Active:    o.active,
		Duration:  o.duration.snapshot(),
		TTFT:      o.ttft.snapshot(),
	}
}

type modelFlight struct {
	started   time.Time
	lastDelta time.Time
	deltaSeen bool
}

// Collector 同步、无 I/O 地聚合 RuntimeEvent；可直接注册到 Bus。
type Collector struct {
	mu sync.Mutex

	model  operation
	tool   operation
	task   operation
	memory operation

	modelFlights              map[string]modelFlight
	memoryFlights             map[string]modelFlight
	tokens                    uint64
	inputTokens               uint64
	cachedTokens              uint64
	cacheWriteTokens          uint64
	toolCalls                 uint64
	modelRetries              uint64
	memoryTokens              uint64
	memoryInputTokens         uint64
	memoryCachedTokens        uint64
	memoryCacheWriteTokens    uint64
	memoryRetries             uint64
	memoryOrganizerRuns       uint64
	memoryOrganizerCandidates uint64
	memoryOrganizerSelected   uint64
	memoryOrganizerTokens     uint64
	memoryOrganizerDuration   histogram
	memoryOrganizerFlights    map[string]uint64
	streamChunks              uint64
	memoryStreamChunks        uint64
	deltaChunks               uint64
	deltaBytes                uint64
}

// NewCollector 创建有界内存采集器。
func NewCollector() *Collector {
	return &Collector{
		modelFlights:           make(map[string]modelFlight),
		memoryFlights:          make(map[string]modelFlight),
		memoryOrganizerFlights: make(map[string]uint64),
	}
}

// Handle 聚合一条事件；它不保留正文，也不执行外部 I/O。
func (c *Collector) Handle(_ context.Context, ev RuntimeEvent) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if ev.Kind == KindModel && ev.Phase == PhaseDelta {
		if ev.Delta == "" {
			c.streamChunks++
		} else {
			c.deltaChunks++
			c.deltaBytes += uint64(len(ev.Delta))
		}
		flight, ok := c.modelFlights[ev.AgentID]
		if ok {
			flight.lastDelta = ev.Time
			if (ev.StreamText || ev.Delta != "") && !flight.deltaSeen && !flight.started.IsZero() && !ev.Time.Before(flight.started) {
				c.model.ttft.observe(ev.Time.Sub(flight.started))
				flight.deltaSeen = true
			}
			c.modelFlights[ev.AgentID] = flight
		}
		return
	}
	if ev.Kind == KindMemory && ev.Phase == PhaseDelta {
		c.memoryStreamChunks++
		flight, ok := c.memoryFlights[ev.AgentID]
		if ok {
			flight.lastDelta = ev.Time
			if ev.StreamText && !flight.deltaSeen && !flight.started.IsZero() && !ev.Time.Before(flight.started) {
				c.memory.ttft.observe(ev.Time.Sub(flight.started))
				flight.deltaSeen = true
			}
			c.memoryFlights[ev.AgentID] = flight
		}
		return
	}

	op := c.operation(ev.Kind)
	if op == nil {
		return
	}
	switch ev.Phase {
	case PhaseStart:
		op.started++
		op.active++
		if ev.Kind == KindModel {
			c.modelFlights[ev.AgentID] = modelFlight{started: ev.Time}
		}
		if ev.Kind == KindMemory {
			if strings.HasPrefix(ev.Name, "organize_") {
				c.memoryOrganizerFlights[ev.AgentID] = 0
			} else {
				c.memoryFlights[ev.AgentID] = modelFlight{started: ev.Time}
			}
		}
	case PhaseEnd:
		op.completed++
		if op.active > 0 {
			op.active--
		}
		if ev.IsError || ev.Err != "" {
			op.errors++
		}
		if ev.Duration >= 0 {
			op.duration.observe(ev.Duration)
		}
		if ev.Kind == KindModel {
			delete(c.modelFlights, ev.AgentID)
			c.tokens += uint64(max(ev.Tokens, 0))
			c.inputTokens += uint64(max(ev.InputTokens, 0))
			c.cachedTokens += uint64(max(ev.CachedTokens, 0))
			c.cacheWriteTokens += uint64(max(ev.CacheWriteTokens, 0))
			c.toolCalls += uint64(max(ev.ToolCalls, 0))
			if _, ok := c.memoryOrganizerFlights[ev.AgentID]; ok {
				c.memoryOrganizerFlights[ev.AgentID] += uint64(max(ev.Tokens, 0))
			}
		}
		if ev.Kind == KindMemory {
			delete(c.memoryFlights, ev.AgentID)
			c.memoryTokens += uint64(max(ev.Tokens, 0))
			c.memoryInputTokens += uint64(max(ev.InputTokens, 0))
			c.memoryCachedTokens += uint64(max(ev.CachedTokens, 0))
			c.memoryCacheWriteTokens += uint64(max(ev.CacheWriteTokens, 0))
			c.memoryRetries += uint64(max(ev.Retries, 0))
			if ev.MemoryOrganized {
				c.memoryOrganizerRuns++
				c.memoryOrganizerCandidates += uint64(max(ev.MemoryCandidates, 0))
				c.memoryOrganizerSelected += uint64(max(ev.MemorySelected, 0))
				c.memoryOrganizerTokens += c.memoryOrganizerFlights[ev.AgentID]
				c.memoryOrganizerDuration.observe(ev.Duration)
			}
			delete(c.memoryOrganizerFlights, ev.AgentID)
		}
	case PhaseRetry:
		if ev.Kind == KindModel {
			c.modelRetries++
		}
	}
}

func (c *Collector) operation(kind Kind) *operation {
	switch kind {
	case KindModel:
		return &c.model
	case KindTool:
		return &c.tool
	case KindTask:
		return &c.task
	case KindMemory:
		return &c.memory
	default:
		return nil
	}
}

// Snapshot 返回并发一致的聚合快照。
func (c *Collector) Snapshot() MetricsSnapshot {
	if c == nil {
		return MetricsSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	modelStreamIdle := streamIdle(now, c.modelFlights)
	memoryStreamIdle := streamIdle(now, c.memoryFlights)
	cacheHitRate := 0.0
	if c.inputTokens > 0 {
		cacheHitRate = float64(c.cachedTokens) / float64(c.inputTokens)
	}
	totalCacheHitRate := 0.0
	totalInputTokens := c.inputTokens + c.memoryInputTokens
	if totalInputTokens > 0 {
		totalCacheHitRate = float64(c.cachedTokens+c.memoryCachedTokens) / float64(totalInputTokens)
	}
	return MetricsSnapshot{
		Model:                     c.model.snapshot(),
		Tool:                      c.tool.snapshot(),
		Task:                      c.task.snapshot(),
		Memory:                    c.memory.snapshot(),
		Tokens:                    c.tokens,
		InputTokens:               c.inputTokens,
		CachedTokens:              c.cachedTokens,
		CacheWriteTokens:          c.cacheWriteTokens,
		CacheHitRate:              cacheHitRate,
		ToolCalls:                 c.toolCalls,
		ModelRetries:              c.modelRetries,
		MemoryTokens:              c.memoryTokens,
		MemoryInputTokens:         c.memoryInputTokens,
		MemoryCachedTokens:        c.memoryCachedTokens,
		MemoryCacheWriteTokens:    c.memoryCacheWriteTokens,
		TotalCacheHitRate:         totalCacheHitRate,
		MemoryRetries:             c.memoryRetries,
		MemoryOrganizerRuns:       c.memoryOrganizerRuns,
		MemoryOrganizerCandidates: c.memoryOrganizerCandidates,
		MemoryOrganizerSelected:   c.memoryOrganizerSelected,
		MemoryOrganizerTokens:     c.memoryOrganizerTokens,
		MemoryOrganizerDuration:   c.memoryOrganizerDuration.snapshot(),
		StreamChunks:              c.streamChunks,
		ModelStreamIdle:           modelStreamIdle,
		MemoryStreamChunks:        c.memoryStreamChunks,
		MemoryStreamIdle:          memoryStreamIdle,
		DeltaChunks:               c.deltaChunks,
		DeltaBytes:                c.deltaBytes,
	}
}

func streamIdle(now time.Time, flights map[string]modelFlight) time.Duration {
	var idle time.Duration
	for _, flight := range flights {
		last := flight.lastDelta
		if last.IsZero() {
			last = flight.started
		}
		if last.IsZero() || now.Before(last) {
			continue
		}
		idle = max(idle, now.Sub(last))
	}
	return idle
}
