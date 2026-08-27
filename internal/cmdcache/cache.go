package cmdcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// maxCandidates 限制一次查找里要校验的条目数。同一命令在不同依赖状态下
	// 会产生多条条目，按最近使用排序后只看最前面几条，避免查找成本无界。
	maxCandidates = 32
	// defaultMaxBytes 是产物存储的默认上限。
	defaultMaxBytes = 2 << 30
	// defaultMaxReadSet 是允许的最大依赖数。读集再大，校验成本就会盖过收益。
	defaultMaxReadSet = 20000
	// gcInterval 是触发一次容量裁剪要经过的写入次数。
	gcInterval = 64
)

// Config 配置命令结果缓存。
type Config struct {
	// Dir 是缓存根目录。跨进程共享靠它，多个 Threadmill 进程可以指同一个目录。
	Dir string
	// MaxBytes 是产物存储上限，超出按最近最少使用回收。
	MaxBytes int64
	// MaxReadSet 是单条命令允许的最大依赖数。
	MaxReadSet int
	// CacheFailures 决定非零退出码的结果是否入缓存。
	// 超时与沙箱错误无论如何都不入缓存：它们反映的是环境，不是命令的结果。
	CacheFailures bool
	// VerifySampleRate 是命中后仍然照常执行、用来对账的比例。
	// 读集是观测得来的，不是证明出来的；没走到的分支的依赖不在集合里。
	// 抽样对账是发现这类静默误命中的唯一手段。
	VerifySampleRate float64
}

// Stats 是缓存的累计计数，供监控消费。只含有界的标量，不含路径或内容。
type Stats struct {
	Lookups          uint64        `json:"lookups"`
	Hits             uint64        `json:"hits"`
	Stores           uint64        `json:"stores"`
	Rejected         uint64        `json:"rejected"`
	ReplayErrors     uint64        `json:"replay_errors"`
	ArtifactReflinks uint64        `json:"artifact_reflinks"`
	ReflinkBytes     uint64        `json:"reflink_bytes"`
	ArtifactCopies   uint64        `json:"artifact_copies"`
	CopiedBytes      uint64        `json:"copied_bytes"`
	Verifications    uint64        `json:"verifications"`
	VerifyMismatches uint64        `json:"verify_mismatches"`
	SavedDuration    time.Duration `json:"saved_duration"`
}

// Cache 按推断出的依赖复用命令执行结果。零值不可用，须经 New 构造。
type Cache struct {
	dir           string
	blobs         blobStore
	maxBytes      int64
	maxReadSet    int
	cacheFailures bool
	verifyRate    float64

	mu      sync.Mutex
	stats   Stats
	sinceGC int
}

// New 打开或创建一个缓存目录。
func New(cfg Config) (*Cache, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("cmdcache: empty cache directory")
	}
	blobDir := filepath.Join(cfg.Dir, "cas")
	if err := os.MkdirAll(blobDir, 0o700); err != nil {
		return nil, fmt.Errorf("cmdcache: create cache directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "index"), 0o700); err != nil {
		return nil, fmt.Errorf("cmdcache: create cache directory: %w", err)
	}
	c := &Cache{
		dir:           cfg.Dir,
		blobs:         blobStore{root: blobDir},
		maxBytes:      cfg.MaxBytes,
		maxReadSet:    cfg.MaxReadSet,
		cacheFailures: cfg.CacheFailures,
		verifyRate:    cfg.VerifySampleRate,
	}
	if c.maxBytes <= 0 {
		c.maxBytes = defaultMaxBytes
	}
	if c.maxReadSet <= 0 {
		c.maxReadSet = defaultMaxReadSet
	}
	return c, nil
}

// Stats 返回累计计数快照。缓存未启用时返回零值。
func (c *Cache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// ShouldVerify 按采样率决定这次命中是否仍然照常执行来对账。
func (c *Cache) ShouldVerify() bool {
	return c != nil && c.verifyRate > 0 && rand.Float64() < c.verifyRate
}

func (c *Cache) indexDir(key Key) string {
	return filepath.Join(c.dir, "index", key.index())
}

// Lookup 找一条依赖在 live 树里仍然成立的条目，没有则返回 nil。
//
// 校验只触碰读集里的那些路径，不扫全树——这正是相对整树指纹的收益来源：
// 别的 agent 改了无关文件不会让这条命令失效。
func (c *Cache) Lookup(live string, key Key) (*Entry, error) {
	c.mu.Lock()
	c.stats.Lookups++
	c.mu.Unlock()

	candidates, err := c.candidates(key)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	for _, name := range candidates {
		full := filepath.Join(c.indexDir(key), name)
		entry, err := loadEntry(full)
		if err != nil {
			// 条目损坏或被并发回收：删掉它继续找下一条。
			_ = os.Remove(full)
			continue
		}
		if !c.artifactsPresent(entry) {
			// 产物被 GC 回收了，这条条目已经没法回放。
			_ = os.Remove(full)
			continue
		}
		ok, err := entry.matches(live)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		now := time.Now()
		_ = os.Chtimes(full, now, now)
		c.mu.Lock()
		c.stats.Hits++
		c.stats.SavedDuration += time.Duration(entry.DurationNS)
		c.mu.Unlock()
		return entry, nil
	}
	return nil, nil
}

// candidates 返回同一 Key 下按最近使用排序的条目文件名，数量有上限。
func (c *Cache) candidates(key Key) ([]string, error) {
	entries, err := os.ReadDir(c.indexDir(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cmdcache: read index: %w", err)
	}
	type candidate struct {
		name    string
		modTime int64
	}
	found := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		found = append(found, candidate{name: entry.Name(), modTime: info.ModTime().UnixNano()})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].modTime > found[j].modTime })
	if len(found) > maxCandidates {
		found = found[:maxCandidates]
	}
	names := make([]string, 0, len(found))
	for _, item := range found {
		names = append(names, item.name)
	}
	return names, nil
}

func (c *Cache) artifactsPresent(entry *Entry) bool {
	for _, change := range entry.Writes {
		if change.Kind == ChangeFile && !c.blobs.has(change.Digest) {
			return false
		}
	}
	return true
}

// Store 记录一次执行结果，返回落盘的条目；观测不可缓存时返回 nil。
//
// 依赖状态在执行之后计算。这对纯输入路径是正确的：命令没有改动它们，
// 执行后读到的就是执行前的值。既读又写的命令由 Observation.Cacheable
// 拦掉，不会走到这里。
func (c *Cache) Store(live string, key Key, obs Observation, result Result) (*Entry, error) {
	if !obs.Cacheable() || len(obs.Reads) > c.maxReadSet {
		c.reject()
		return nil, nil
	}
	if result.ExitCode != 0 && !c.cacheFailures {
		c.reject()
		return nil, nil
	}
	managed := make([]string, 0, len(obs.Writes))
	for rel := range obs.Writes {
		managed = append(managed, rel)
	}
	sort.Strings(managed)

	entry := &Entry{
		Command:    key.Command,
		Backend:    key.Backend,
		EnvHash:    key.EnvHash,
		Reads:      make(map[string]string, len(obs.Reads)),
		Managed:    managed,
		ExitCode:   result.ExitCode,
		Output:     result.Output,
		DurationNS: int64(result.Duration),
		CreatedAt:  time.Now().UnixNano(),
	}
	managedSet := entry.managedSet()
	for rel, kind := range obs.Reads {
		if kind == ReadAbsent {
			// 追踪当场就观测到它不存在，这就是精确的执行前状态。
			// 不能改用执行后的 lstat：命令可能正是为了创建它才先探测的。
			entry.Reads[rel] = stateAbsent
			continue
		}
		state, err := readState(live, rel, kind, managedSet)
		if err != nil {
			c.reject()
			return nil, nil //nolint:nilerr // 依赖不可读时放弃缓存，不影响本次执行
		}
		entry.Reads[rel] = state
	}
	if len(obs.Externals) > 0 {
		entry.Externals = make(map[string]string, len(obs.Externals))
		for _, abs := range obs.Externals {
			entry.Externals[abs] = externalState(abs)
		}
	}
	changes, err := c.captureArtifacts(live, managed)
	if err != nil {
		c.reject()
		return nil, err
	}
	entry.Writes = changes
	entry.ID = entry.fingerprint()
	if err := c.writeEntry(key, entry); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.stats.Stores++
	c.sinceGC++
	due := c.sinceGC >= gcInterval
	if due {
		c.sinceGC = 0
	}
	c.mu.Unlock()
	if due {
		_ = c.GC()
	}
	return entry, nil
}

// captureArtifacts 按写集逐条读回产物。代价是 O(写集)，不是 O(整棵树)——
// 写集来自系统调用追踪，已经精确到路径，不需要再扫描 live 树做差分。
func (c *Cache) captureArtifacts(live string, managed []string) ([]Change, error) {
	changes := make([]Change, 0, len(managed))
	for _, rel := range managed {
		full, err := resolveRel(live, rel)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(full)
		if err != nil {
			if os.IsNotExist(err) {
				changes = append(changes, Change{Path: rel, Kind: ChangeDelete})
				continue
			}
			return nil, fmt.Errorf("cmdcache: stat artifact %q: %w", rel, err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return nil, fmt.Errorf("cmdcache: readlink %q: %w", rel, err)
			}
			changes = append(changes, Change{Path: rel, Kind: ChangeSymlink, Target: target})
		case info.IsDir():
			changes = append(changes, Change{Path: rel, Kind: ChangeDir})
		case info.Mode().IsRegular():
			digest, err := c.blobs.putFile(full)
			if err != nil {
				return nil, err
			}
			changes = append(changes, Change{
				Path:       rel,
				Kind:       ChangeFile,
				Digest:     digest,
				Executable: info.Mode()&0o111 != 0,
			})
		default:
			// 设备、FIFO、socket 复制不过去，这次结果放弃缓存。
			return nil, fmt.Errorf("cmdcache: artifact %q has unsupported type %s", rel, info.Mode().Type())
		}
	}
	return changes, nil
}

// readState 按依赖的种类计算它的状态串：只被 stat 过的路径记类型，
// 真正被读过的记内容。这个区分决定了无关文件的编辑会不会让缓存失效。
func readState(live, rel string, kind ReadKind, managed map[string]struct{}) (string, error) {
	if kind == ReadStat {
		return pathTypeState(live, rel)
	}
	return pathStateExcluding(live, rel, managed)
}

func (c *Cache) reject() {
	c.mu.Lock()
	c.stats.Rejected++
	c.mu.Unlock()
}

func (c *Cache) writeEntry(key Key, entry *Entry) error {
	dir := c.indexDir(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cmdcache: create index directory: %w", err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("cmdcache: encode entry: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return fmt.Errorf("cmdcache: stage entry: %w", err)
	}
	staged := temp.Name()
	defer os.Remove(staged)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("cmdcache: stage entry: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("cmdcache: stage entry: %w", err)
	}
	// rename 是原子的：并发的另一个进程要么读到旧条目，要么读到完整的新条目。
	if err := os.Rename(staged, filepath.Join(dir, entry.ID+".json")); err != nil {
		return fmt.Errorf("cmdcache: commit entry: %w", err)
	}
	return nil
}

func loadEntry(full string) (*Entry, error) {
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, err
	}
	entry.ID = strip(filepath.Base(full), ".json")
	return &entry, nil
}

func strip(name, suffix string) string {
	if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
		return name[:len(name)-len(suffix)]
	}
	return name
}

// Invalidate 作废一条条目。抽样对账发现结果不一致时调用。
func (c *Cache) Invalidate(key Key, entry *Entry) error {
	c.mu.Lock()
	c.stats.VerifyMismatches++
	c.mu.Unlock()
	err := os.Remove(filepath.Join(c.indexDir(key), entry.ID+".json"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cmdcache: invalidate entry: %w", err)
	}
	return nil
}

// Replay 把条目的产物写进 live 树。
//
// 不需要额外的前置条件：写集里的路径都是命令首次触碰即写入的，也就是它本来
// 就会无条件覆盖的路径，回放覆盖它们与真跑一遍等价。真正被命令读过的产物
// 路径会同时出现在读集里，那种命令在记录阶段就被判成不可缓存了。
func (c *Cache) Replay(live string, entry *Entry) error {
	if err := c.replay(live, entry); err != nil {
		c.mu.Lock()
		c.stats.ReplayErrors++
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *Cache) replay(live string, entry *Entry) error {
	var deletes, dirs, files []Change
	for _, change := range entry.Writes {
		switch change.Kind {
		case ChangeDelete:
			deletes = append(deletes, change)
		case ChangeDir:
			dirs = append(dirs, change)
		default:
			files = append(files, change)
		}
	}
	// 先删后建，目录按字典序建（父目录总是子目录的前缀，所以先于子目录出现）。
	for _, change := range deletes {
		full, err := resolveRel(live, change.Path)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(full); err != nil {
			return fmt.Errorf("cmdcache: replay delete %q: %w", change.Path, err)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Path < dirs[j].Path })
	for _, change := range dirs {
		full, err := resolveRel(live, change.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(full, 0o755); err != nil {
			return fmt.Errorf("cmdcache: replay mkdir %q: %w", change.Path, err)
		}
	}
	for _, change := range files {
		if err := c.replayFile(live, change); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cache) replayFile(live string, change Change) error {
	full, err := resolveRel(live, change.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("cmdcache: replay parent of %q: %w", change.Path, err)
	}
	if change.Kind == ChangeSymlink {
		if err := os.RemoveAll(full); err != nil {
			return fmt.Errorf("cmdcache: replay symlink %q: %w", change.Path, err)
		}
		if err := os.Symlink(change.Target, full); err != nil {
			return fmt.Errorf("cmdcache: replay symlink %q: %w", change.Path, err)
		}
		return nil
	}
	source, err := c.blobs.open(change.Digest)
	if err != nil {
		return fmt.Errorf("cmdcache: replay %q: %w", change.Path, err)
	}
	sourceInfo, err := source.Stat()
	if err != nil {
		return errors.Join(
			fmt.Errorf("cmdcache: stat artifact %q: %w", change.Path, err),
			source.Close(),
		)
	}
	// 回放即使用：更新 blob 的时间戳，让热产物在容量裁剪里活下来。
	now := time.Now()
	_ = os.Chtimes(c.blobs.path(change.Digest), now, now)

	mode := os.FileMode(0o644)
	if change.Executable {
		mode = 0o755
	}
	temp, err := os.CreateTemp(filepath.Dir(full), ".tmcache-")
	if err != nil {
		return errors.Join(
			fmt.Errorf("cmdcache: replay %q: %w", change.Path, err),
			source.Close(),
		)
	}
	staged := temp.Name()
	defer os.Remove(staged)
	reflinked, materializeErr := cloneOrCopy(temp, source)
	if err := errors.Join(materializeErr, source.Close(), temp.Close()); err != nil {
		return fmt.Errorf("cmdcache: replay %q: %w", change.Path, err)
	}
	if err := os.Chmod(staged, mode); err != nil {
		return fmt.Errorf("cmdcache: replay %q: %w", change.Path, err)
	}
	// 目标可能是目录，rename 覆盖不了，先清掉。
	if info, err := os.Lstat(full); err == nil && info.IsDir() {
		if err := os.RemoveAll(full); err != nil {
			return fmt.Errorf("cmdcache: replay %q: %w", change.Path, err)
		}
	}
	if err := os.Rename(staged, full); err != nil {
		return fmt.Errorf("cmdcache: replay %q: %w", change.Path, err)
	}
	c.mu.Lock()
	if reflinked {
		c.stats.ArtifactReflinks++
		c.stats.ReflinkBytes += uint64(sourceInfo.Size())
	} else {
		c.stats.ArtifactCopies++
		c.stats.CopiedBytes += uint64(sourceInfo.Size())
	}
	c.mu.Unlock()
	return nil
}

func cloneOrCopy(target, source *os.File) (bool, error) {
	if err := unix.IoctlFileClone(int(target.Fd()), int(source.Fd())); err == nil {
		return true, nil
	}
	if err := target.Truncate(0); err != nil {
		return false, fmt.Errorf("reset copy target: %w", err)
	}
	if _, err := target.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("seek copy target: %w", err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("seek artifact: %w", err)
	}
	if _, err := io.Copy(target, source); err != nil {
		return false, fmt.Errorf("copy artifact: %w", err)
	}
	return false, nil
}

// RecordVerification 记一次抽样对账。
func (c *Cache) RecordVerification() {
	c.mu.Lock()
	c.stats.Verifications++
	c.mu.Unlock()
}

// GC 把产物存储裁到容量上限以内，按最近最少使用回收。
// 被回收产物对应的条目会在下次查找时被清掉。
func (c *Cache) GC() error {
	blobs, total, err := c.blobs.collect()
	if err != nil {
		return err
	}
	if total <= c.maxBytes {
		return nil
	}
	// 裁到上限的八成，避免每次写入都触发回收。
	target := c.maxBytes / 10 * 8
	for _, blob := range blobs {
		if total <= target {
			break
		}
		if err := os.Remove(blob.path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("cmdcache: reclaim blob: %w", err)
		}
		total -= blob.size
	}
	return nil
}

// EnvHash 把影响执行的环境变量摘要成一个稳定的串。
// 同一命令在不同环境变量下结果可能不同，所以它属于 Key 而不是读集。
func EnvHash(values []string) string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	hasher := sha256.New()
	for _, value := range sorted {
		fmt.Fprintf(hasher, "%d:%s\n", len(value), value)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
