package tool

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

// 参数形状对齐 earendil-works/pi packages/coding-agent/src/core/tools/。
// Threadmill：路径原样交给 FileView（jail 已拒绝 .. / abs）；read 只出文本；grep/find 进程内走 List/Read。
const (
	fileReadName  = "read"
	fileWriteName = "write"
	fileEditName  = "edit"
	fileLsName    = "ls"
	fileGrepName  = "grep"
	fileFindName  = "find"

	fileReadMaxLines     = 2000
	fileReadMaxBytes     = 50 * 1024
	fileLsDefaultLimit   = 500
	fileGrepDefaultLimit = 100
	fileGrepMaxLineBytes = 4 * 1024
	fileGrepMaxBytes     = 50 * 1024
	fileFindDefaultLimit = 1000
)

var errFileWalkDone = errors.New("file walk done")

type fileTool struct {
	name  string
	files env.FileView
}

var (
	_ Tool      = fileTool{}
	_ EnvBinder = fileTool{}
)

// FileTools 返回绑到 Env.Files 的工作区文件工具。未 BindEnv 时 Execute 报基础设施错误。
func FileTools() []Tool {
	return []Tool{
		fileTool{name: fileReadName},
		fileTool{name: fileWriteName},
		fileTool{name: fileEditName},
		fileTool{name: fileLsName},
		fileTool{name: fileGrepName},
		fileTool{name: fileFindName},
	}
}

func (t fileTool) BindEnv(e env.Env) Tool {
	t.files = e.Files
	return t
}

func (t fileTool) Definition() Definition {
	switch t.name {
	case fileReadName:
		return Definition{
			Name:        fileReadName,
			Description: "读取工作区文本文件。offset 是从 1 起的行号，limit 是最大行数。约 2000 行或 50KB 截断；截断时给出下一个 offset。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"要读的文件路径"},"offset":{"type":"integer","description":"起始行号（从 1 起）"},"limit":{"type":"integer","description":"最多读取的行数"}},"required":["path"],"additionalProperties":false}`),
		}
	case fileWriteName:
		return Definition{
			Name:        fileWriteName,
			Description: "写入文件：不存在则创建，存在则覆盖。父目录只是路径前缀，直接写出文件键。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"要写的文件路径"},"content":{"type":"string","description":"文件内容"}},"required":["path","content"],"additionalProperties":false}`),
		}
	case fileEditName:
		return Definition{
			Name:        fileEditName,
			Description: "按精确文本替换编辑一个文件。每条 edits[].oldText 必须在原文件中唯一且互不重叠。也接受顶层 oldText/newText 作为单条编辑。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"要编辑的文件路径"},"edits":{"type":"array","items":{"type":"object","properties":{"oldText":{"type":"string"},"newText":{"type":"string"}},"required":["oldText","newText"]},"description":"针对原文件的替换列表"},"oldText":{"type":"string","description":"兼容单次替换的旧文本"},"newText":{"type":"string","description":"兼容单次替换的新文本"}},"required":["path"],"additionalProperties":false}`),
		}
	case fileLsName:
		return Definition{
			Name:        fileLsName,
			Description: "列出目录。目录名带 / 后缀。包含点文件。默认最多 500 条。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"目录路径，默认当前目录"},"limit":{"type":"integer","description":"最多返回的条目数，默认 500"}},"additionalProperties":false}`),
		}
	case fileGrepName:
		return Definition{
			Name:        fileGrepName,
			Description: "在工作区文件中搜索模式。进程内走 FileView 的 List/Read，不调用 rg。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"搜索模式（正则或字面量）"},"path":{"type":"string","description":"搜索的文件或目录，默认当前目录"},"glob":{"type":"string","description":"按 glob 过滤文件"},"ignoreCase":{"type":"boolean","description":"忽略大小写"},"literal":{"type":"boolean","description":"把 pattern 当字面量"},"context":{"type":"integer","description":"匹配行前后各显示的行数"},"limit":{"type":"integer","description":"最多返回的匹配数，默认 100"}},"required":["pattern"],"additionalProperties":false}`),
		}
	default:
		return Definition{
			Name:        fileFindName,
			Description: "按 glob 查找文件。进程内走 FileView，不调用 fd。匹配相对搜索根的路径。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"glob 模式，例如 *.go"},"path":{"type":"string","description":"搜索目录，默认当前目录"},"limit":{"type":"integer","description":"最多返回的路径数，默认 1000"}},"required":["pattern"],"additionalProperties":false}`),
		}
	}
}

func (t fileTool) Execute(ctx context.Context, call Call) (Output, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	if t.files == nil {
		return Output{}, fmt.Errorf("%s: not bound to env", t.name)
	}
	switch t.name {
	case fileReadName:
		return t.read(call.Arguments)
	case fileWriteName:
		return t.write(call.Arguments)
	case fileEditName:
		return t.edit(call.Arguments)
	case fileLsName:
		return t.ls(call.Arguments)
	case fileGrepName:
		return t.grep(ctx, call.Arguments)
	default:
		return t.find(ctx, call.Arguments)
	}
}

func (t fileTool) read(raw json.RawMessage) (Output, error) {
	var args struct {
		Path   string `json:"path"`
		Offset *int   `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	if err := decodeMemoryArgs(raw, &args); err != nil {
		return Output{}, err
	}
	if args.Path == "" {
		return Output{}, fmt.Errorf("%s: missing path", t.name)
	}
	data, err := t.files.Read(args.Path)
	if err != nil {
		return Output{}, fmt.Errorf("%s: %w", t.name, err)
	}

	lines := splitFileLines(string(data))
	start := 0
	if args.Offset != nil && *args.Offset > 0 {
		start = *args.Offset - 1
	}
	if start >= len(lines) {
		offset := 1
		if args.Offset != nil {
			offset = *args.Offset
		}
		return Output{}, fmt.Errorf("%s: offset %d is beyond end of file (%d lines)", t.name, offset, len(lines))
	}

	selected := lines[start:]
	userLimited := false
	if args.Limit != nil && *args.Limit >= 0 && *args.Limit < len(selected) {
		selected = selected[:*args.Limit]
		userLimited = true
	}

	joined := strings.Join(selected, "\n")
	head, truncated, byLines, outputLines := truncateHead(joined, fileReadMaxLines, fileReadMaxBytes)
	startDisplay := start + 1
	if outputLines == 0 && truncated {
		return Output{Content: fmt.Sprintf("[line %d exceeds %d byte limit]", startDisplay, fileReadMaxBytes)}, nil
	}
	if truncated {
		endDisplay := startDisplay + outputLines - 1
		next := endDisplay + 1
		if byLines {
			head += fmt.Sprintf("\n\n[showing lines %d-%d of %d. use offset=%d to continue]", startDisplay, endDisplay, len(lines), next)
		} else {
			head += fmt.Sprintf("\n\n[showing lines %d-%d of %d (%d byte limit). use offset=%d to continue]", startDisplay, endDisplay, len(lines), fileReadMaxBytes, next)
		}
		return Output{Content: head}, nil
	}
	if userLimited && start+len(selected) < len(lines) {
		remaining := len(lines) - (start + len(selected))
		next := start + len(selected) + 1
		head += fmt.Sprintf("\n\n[%d more lines in file. use offset=%d to continue]", remaining, next)
	}
	return Output{Content: head}, nil
}

func (t fileTool) write(raw json.RawMessage) (Output, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeMemoryArgs(raw, &args); err != nil {
		return Output{}, err
	}
	if args.Path == "" {
		return Output{}, fmt.Errorf("%s: missing path", t.name)
	}
	if err := t.files.Write(args.Path, []byte(args.Content)); err != nil {
		return Output{}, fmt.Errorf("%s: %w", t.name, err)
	}
	return Output{Content: fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path)}, nil
}

func (t fileTool) edit(raw json.RawMessage) (Output, error) {
	var args struct {
		Path    string     `json:"path"`
		Edits   []textEdit `json:"edits"`
		OldText string     `json:"oldText"`
		NewText string     `json:"newText"`
	}
	if err := decodeMemoryArgs(raw, &args); err != nil {
		return Output{}, err
	}
	if args.Path == "" {
		return Output{}, fmt.Errorf("%s: missing path", t.name)
	}
	if args.OldText != "" || args.NewText != "" {
		args.Edits = append(args.Edits, textEdit{OldText: args.OldText, NewText: args.NewText})
	}
	data, err := t.files.Read(args.Path)
	if err != nil {
		return Output{}, fmt.Errorf("%s: %w", t.name, err)
	}
	updated, err := applyUniqueEdits(string(data), args.Edits)
	if err != nil {
		return Output{}, err
	}
	if err := t.files.Write(args.Path, []byte(updated)); err != nil {
		return Output{}, fmt.Errorf("%s: %w", t.name, err)
	}
	return Output{Content: fmt.Sprintf("replaced %d block(s) in %s", len(args.Edits), args.Path)}, nil
}

func (t fileTool) ls(raw json.RawMessage) (Output, error) {
	var args struct {
		Path  string `json:"path"`
		Limit *int   `json:"limit"`
	}
	if err := decodeMemoryArgs(raw, &args); err != nil {
		return Output{}, err
	}
	dir := args.Path
	if dir == "" {
		dir = "."
	}
	limit := fileLsDefaultLimit
	if args.Limit != nil && *args.Limit > 0 {
		limit = *args.Limit
	}
	ents, err := t.files.List(dir)
	if err != nil {
		return Output{}, fmt.Errorf("%s: %w", t.name, err)
	}
	slices.SortFunc(ents, func(a, b env.DirEnt) int {
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	names := make([]string, 0, len(ents))
	for _, ent := range ents {
		name := ent.Name
		if ent.IsDir {
			name += "/"
		}
		names = append(names, name)
		if len(names) >= limit {
			break
		}
	}
	if len(names) == 0 {
		return Output{Content: "(empty directory)"}, nil
	}
	return Output{Content: strings.Join(names, "\n")}, nil
}

func (t fileTool) grep(ctx context.Context, raw json.RawMessage) (Output, error) {
	var args struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignoreCase"`
		Literal    bool   `json:"literal"`
		Context    int    `json:"context"`
		Limit      *int   `json:"limit"`
	}
	if err := decodeMemoryArgs(raw, &args); err != nil {
		return Output{}, err
	}
	if args.Pattern == "" {
		return Output{}, fmt.Errorf("%s: missing pattern", t.name)
	}
	root := args.Path
	if root == "" {
		root = "."
	}
	limit := fileGrepDefaultLimit
	if args.Limit != nil && *args.Limit > 0 {
		limit = *args.Limit
	}
	expr := args.Pattern
	if args.Literal {
		expr = regexp.QuoteMeta(expr)
	}
	if args.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Output{}, fmt.Errorf("%s: invalid pattern: %w", t.name, err)
	}

	var hits []string
	var matches int
	used := 0
	capped := false
	err = walkFiles(ctx, t.files, root, true, func(full, rel string, data []byte) error {
		if args.Glob != "" && !matchGlob(args.Glob, rel) && !matchGlob(args.Glob, full) {
			return nil
		}
		lines := splitFileLines(string(data))
		ctxLines := args.Context
		if ctxLines < 0 {
			ctxLines = 0
		}
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			matches++
			lineNo := i + 1
			var rows []string
			if ctxLines == 0 {
				rows = []string{fmt.Sprintf("%s:%d: %s", rel, lineNo, line)}
			} else {
				from := lineNo - ctxLines
				if from < 1 {
					from = 1
				}
				to := lineNo + ctxLines
				if to > len(lines) {
					to = len(lines)
				}
				for n := from; n <= to; n++ {
					if n == lineNo {
						rows = append(rows, fmt.Sprintf("%s:%d: %s", rel, n, lines[n-1]))
					} else {
						rows = append(rows, fmt.Sprintf("%s-%d- %s", rel, n, lines[n-1]))
					}
				}
			}
			for _, row := range rows {
				next, stop, hitCap := appendBoundedLine(hits, row, used)
				hits = next
				used = 0
				if len(hits) > 0 {
					used = len(strings.Join(hits, "\n"))
				}
				if hitCap {
					capped = true
				}
				if stop {
					return errFileWalkDone
				}
			}
			if matches >= limit {
				return errFileWalkDone
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errFileWalkDone) {
		return Output{}, err
	}
	if len(hits) == 0 {
		return Output{Content: "no matches found"}, nil
	}
	out := strings.Join(hits, "\n")
	if capped && !strings.Contains(out, "truncated") {
		out += "\n[grep output truncated]"
	}
	return Output{Content: out}, nil
}

func (t fileTool) find(ctx context.Context, raw json.RawMessage) (Output, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Limit   *int   `json:"limit"`
	}
	if err := decodeMemoryArgs(raw, &args); err != nil {
		return Output{}, err
	}
	if args.Pattern == "" {
		return Output{}, fmt.Errorf("%s: missing pattern", t.name)
	}
	root := args.Path
	if root == "" {
		root = "."
	}
	limit := fileFindDefaultLimit
	if args.Limit != nil && *args.Limit > 0 {
		limit = *args.Limit
	}

	var hits []string
	err := walkFiles(ctx, t.files, root, false, func(full, rel string, _ []byte) error {
		if !matchGlob(args.Pattern, rel) && !matchGlob(args.Pattern, full) {
			return nil
		}
		hits = append(hits, rel)
		if len(hits) >= limit {
			return errFileWalkDone
		}
		return nil
	})
	if err != nil && !errors.Is(err, errFileWalkDone) {
		return Output{}, err
	}
	if len(hits) == 0 {
		return Output{Content: "no files found matching pattern"}, nil
	}
	return Output{Content: strings.Join(hits, "\n")}, nil
}

type textEdit struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

func applyUniqueEdits(content string, edits []textEdit) (string, error) {
	if len(edits) == 0 {
		return "", fmt.Errorf("edit: missing edits")
	}
	type span struct {
		start, end int
		newText    string
	}
	spans := make([]span, 0, len(edits))
	for _, edit := range edits {
		if edit.OldText == "" {
			return "", fmt.Errorf("edit: empty oldText")
		}
		n := strings.Count(content, edit.OldText)
		if n == 0 {
			return "", fmt.Errorf("edit: oldText not found")
		}
		if n > 1 {
			return "", fmt.Errorf("edit: oldText is not unique")
		}
		start := strings.Index(content, edit.OldText)
		spans = append(spans, span{
			start:   start,
			end:     start + len(edit.OldText),
			newText: edit.NewText,
		})
	}
	slices.SortFunc(spans, func(a, b span) int {
		return cmp.Compare(a.start, b.start)
	})
	for i := 1; i < len(spans); i++ {
		if spans[i-1].end > spans[i].start {
			return "", fmt.Errorf("edit: overlapping edits")
		}
	}
	var b strings.Builder
	pos := 0
	for _, s := range spans {
		b.WriteString(content[pos:s.start])
		b.WriteString(s.newText)
		pos = s.end
	}
	b.WriteString(content[pos:])
	return b.String(), nil
}

func walkFiles(ctx context.Context, files env.FileView, root string, contents bool, fn func(full, rel string, data []byte) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if contents {
		data, err := files.Read(root)
		if err == nil {
			return fn(root, displayRel(root, root), data)
		}
	} else {
		info, err := files.Stat(root)
		if err != nil {
			return err
		}
		if !info.IsDir {
			return fn(root, displayRel(root, root), nil)
		}
	}
	return walkDir(ctx, files, root, root, contents, fn)
}

func walkDir(ctx context.Context, files env.FileView, root, dir string, contents bool, fn func(full, rel string, data []byte) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ents, err := files.List(dir)
	if err != nil {
		if dir == root {
			return err
		}
		return nil
	}
	slices.SortFunc(ents, func(a, b env.DirEnt) int {
		return cmp.Compare(a.Name, b.Name)
	})
	for _, ent := range ents {
		if ent.Name == "" || ent.Name == "." || ent.Name == ".." {
			continue
		}
		full := joinFilePath(dir, ent.Name)
		if ent.IsDir {
			if err := walkDir(ctx, files, root, full, contents, fn); err != nil {
				return err
			}
			continue
		}
		if !contents {
			if err := fn(full, displayRel(root, full), nil); err != nil {
				return err
			}
			continue
		}
		data, err := files.Read(full)
		if err != nil {
			continue
		}
		if err := fn(full, displayRel(root, full), data); err != nil {
			return err
		}
	}
	return nil
}

func appendBoundedLine(hits []string, line string, used int) ([]string, bool, bool) {
	capped := false
	if len(line) > fileGrepMaxLineBytes {
		line = line[:fileGrepMaxLineBytes] + "…"
		capped = true
	}
	add := len(line)
	if len(hits) > 0 {
		add++
	}
	if used+add > fileGrepMaxBytes {
		if len(hits) == 0 {
			hits = append(hits, line[:min(len(line), fileGrepMaxBytes)]+"\n[grep output truncated]")
			return hits, true, true
		}
		hits = append(hits, "[grep output truncated]")
		return hits, true, true
	}
	return append(hits, line), false, capped
}

func joinFilePath(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	return strings.TrimSuffix(dir, "/") + "/" + name
}

func displayRel(root, full string) string {
	if root == "" || root == "." || root == full {
		return full
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if strings.HasPrefix(full, prefix) {
		return full[len(prefix):]
	}
	return full
}

func matchGlob(pattern, name string) bool {
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	name = strings.ReplaceAll(name, "\\", "/")
	if pattern == "" {
		return false
	}
	if strings.Contains(pattern, "**") {
		re, err := globRegexp(pattern)
		if err != nil {
			return false
		}
		return re.MatchString(name)
	}
	if !strings.Contains(pattern, "/") {
		ok, err := path.Match(pattern, path.Base(name))
		return err == nil && ok
	}
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		if i+1 < len(pattern) && pattern[i] == '*' && pattern[i+1] == '*' {
			if i+2 < len(pattern) && pattern[i+2] == '/' {
				b.WriteString("(?:.*/)?")
				i += 2
				continue
			}
			b.WriteString(".*")
			i++
			continue
		}
		switch pattern[i] {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := i + 1
			if end < len(pattern) && (pattern[end] == '!' || pattern[end] == '^') {
				end++
			}
			for end < len(pattern) && pattern[end] != ']' {
				end++
			}
			if end >= len(pattern) {
				b.WriteString(`\[`)
				continue
			}
			class := pattern[i : end+1]
			if len(class) > 2 && class[1] == '!' {
				class = "[^" + class[2:]
			}
			b.WriteString(class)
			i = end
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', ']':
			b.WriteByte('\\')
			b.WriteByte(pattern[i])
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteByte(pattern[i])
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func splitFileLines(content string) []string {
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		return lines[:len(lines)-1]
	}
	return lines
}

func truncateHead(content string, maxLines, maxBytes int) (out string, truncated, byLines bool, outputLines int) {
	lines := splitFileLines(content)
	if content == "" {
		lines = nil
	}
	totalBytes := len(content)
	if len(lines) <= maxLines && totalBytes <= maxBytes {
		return content, false, false, len(lines)
	}
	if len(lines) > 0 && len(lines[0]) > maxBytes {
		return "", true, false, 0
	}
	var b strings.Builder
	n := 0
	used := 0
	for i, line := range lines {
		if n >= maxLines {
			return b.String(), true, true, n
		}
		add := len(line)
		if i > 0 {
			add++
		}
		if used+add > maxBytes {
			return b.String(), true, false, n
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		used += add
		n++
	}
	return b.String(), false, false, n
}
