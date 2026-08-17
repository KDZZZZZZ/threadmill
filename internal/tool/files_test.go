package tool

import (
	"fmt"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

// 参数形状对齐 earendil-works/pi packages/coding-agent/src/core/tools/。
func TestFileToolsDefinitions(t *testing.T) {
	t.Parallel()

	tools := FileTools()
	want := []string{"read", "write", "edit", "ls", "grep", "find"}
	if len(tools) != len(want) {
		t.Fatalf("FileTools() len = %d, want %d", len(tools), len(want))
	}
	for i, tool := range tools {
		def := tool.Definition()
		if def.Name != want[i] {
			t.Fatalf("tool %d name = %q, want %q", i, def.Name, want[i])
		}
		if err := def.Validate(); err != nil {
			t.Fatalf("%s.Validate() = %v", def.Name, err)
		}
		schema := string(def.InputSchema)
		if strings.Contains(schema, `"env"`) || strings.Contains(schema, `"cwd"`) {
			t.Fatalf("%s schema must not contain env or cwd: %s", def.Name, schema)
		}
	}
}

func TestFileToolsUnboundExecuteError(t *testing.T) {
	t.Parallel()

	_, err := executeNamed(t, FileTools(), "read", `{"path":"a.txt"}`)
	if err == nil || !strings.Contains(err.Error(), "not bound to env") {
		t.Fatalf("unbound Execute error = %v, want not bound to env", err)
	}
}

func TestBindEnvSwapsFileView(t *testing.T) {
	t.Parallel()

	one := newFakeFiles(map[string]string{"a.txt": "one"})
	two := newFakeFiles(map[string]string{"a.txt": "two"})

	tools := BindEnv(env.Open("env-1", nil).WithFiles(one), FileTools())
	out, err := executeNamed(t, tools, "read", `{"path":"a.txt"}`)
	if err != nil {
		t.Fatalf("read env-1: %v", err)
	}
	if !strings.Contains(out.Content, "one") {
		t.Fatalf("read env-1 = %q, want one", out.Content)
	}

	tools = BindEnv(env.Open("env-2", nil).WithFiles(two), tools)
	out, err = executeNamed(t, tools, "read", `{"path":"a.txt"}`)
	if err != nil {
		t.Fatalf("read env-2: %v", err)
	}
	if !strings.Contains(out.Content, "two") || strings.Contains(out.Content, "one") {
		t.Fatalf("BindEnv did not swap Files: %q", out.Content)
	}
}

func TestFileToolsIsolateTwoEnvs(t *testing.T) {
	t.Parallel()

	filesA := newFakeFiles(map[string]string{"note.txt": "a"})
	filesB := newFakeFiles(map[string]string{"note.txt": "b"})
	toolsA := BindEnv(env.Open("env-a", nil).WithFiles(filesA), FileTools())
	toolsB := BindEnv(env.Open("env-b", nil).WithFiles(filesB), FileTools())

	if _, err := executeNamed(t, toolsA, "write", `{"path":"note.txt","content":"from-a"}`); err != nil {
		t.Fatalf("write env-a: %v", err)
	}

	outB, err := executeNamed(t, toolsB, "read", `{"path":"note.txt"}`)
	if err != nil {
		t.Fatalf("read env-b: %v", err)
	}
	if strings.Contains(outB.Content, "from-a") {
		t.Fatalf("env-b saw env-a write: %s", outB.Content)
	}
	if !strings.Contains(outB.Content, "b") {
		t.Fatalf("env-b lost its own file: %s", outB.Content)
	}

	outA, err := executeNamed(t, toolsA, "read", `{"path":"note.txt"}`)
	if err != nil {
		t.Fatalf("read env-a: %v", err)
	}
	if !strings.Contains(outA.Content, "from-a") {
		t.Fatalf("env-a did not see its write: %s", outA.Content)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	t.Parallel()

	files := newFakeFiles(map[string]string{"n.txt": "l1\nl2\nl3\nl4"})
	tools := BindEnv(env.Open("env-1", nil).WithFiles(files), FileTools())
	out, err := executeNamed(t, tools, "read", `{"path":"n.txt","offset":2,"limit":2}`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body, _, _ := strings.Cut(out.Content, "\n\n[")
	if body != "l2\nl3" {
		t.Fatalf("read = %q, want l2\\nl3", body)
	}
	if !strings.Contains(out.Content, "offset=4") {
		t.Fatalf("truncated read should tell next offset: %q", out.Content)
	}
}

func TestEditUniqueReplace(t *testing.T) {
	t.Parallel()

	files := newFakeFiles(map[string]string{"a.txt": "hello world"})
	tools := BindEnv(env.Open("env-1", nil).WithFiles(files), FileTools())
	if _, err := executeNamed(t, tools, "edit", `{"path":"a.txt","edits":[{"oldText":"hello","newText":"goodbye"}]}`); err != nil {
		t.Fatalf("edit: %v", err)
	}
	out, err := executeNamed(t, tools, "read", `{"path":"a.txt"}`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(out.Content) != "goodbye world" {
		t.Fatalf("after edit = %q, want goodbye world", out.Content)
	}
}

func TestEditDuplicateOldTextErrors(t *testing.T) {
	t.Parallel()

	files := newFakeFiles(map[string]string{"a.txt": "foo bar foo"})
	tools := BindEnv(env.Open("env-1", nil).WithFiles(files), FileTools())
	_, err := executeNamed(t, tools, "edit", `{"path":"a.txt","oldText":"foo","newText":"baz"}`)
	if err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("duplicate oldText error = %v, want unique", err)
	}
}

func TestLsDirSuffix(t *testing.T) {
	t.Parallel()

	files := newFakeFiles(map[string]string{
		".hidden":    "x",
		"readme.md":  "docs",
		"pkg/lib.go": "package pkg",
	})
	tools := BindEnv(env.Open("env-1", nil).WithFiles(files), FileTools())
	out, err := executeNamed(t, tools, "ls", `{}`)
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	got := strings.Split(strings.TrimSpace(out.Content), "\n")
	want := []string{".hidden", "pkg/", "readme.md"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ls = %q, want %q", got, want)
	}
}

func TestGrepAndFindBasicHit(t *testing.T) {
	t.Parallel()

	files := newFakeFiles(map[string]string{
		"hello.txt": "hello world",
		"other.md":  "nope",
		"dir/n.txt": "hello again",
	})
	tools := BindEnv(env.Open("env-1", nil).WithFiles(files), FileTools())

	grepOut, err := executeNamed(t, tools, "grep", `{"pattern":"hello"}`)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(grepOut.Content, "hello.txt:1:") || !strings.Contains(grepOut.Content, "dir/n.txt:1:") {
		t.Fatalf("grep missed hits: %q", grepOut.Content)
	}

	findOut, err := executeNamed(t, tools, "find", `{"pattern":"*.txt"}`)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !strings.Contains(findOut.Content, "hello.txt") || !strings.Contains(findOut.Content, "dir/n.txt") {
		t.Fatalf("find missed hits: %q", findOut.Content)
	}
	if strings.Contains(findOut.Content, "other.md") {
		t.Fatalf("find matched non-txt: %q", findOut.Content)
	}
}

func TestFileToolsPassPathThrough(t *testing.T) {
	t.Parallel()

	files := newFakeFiles(map[string]string{"ok.txt": "ok"})
	files.reject = func(p string) error {
		if p == ".." || strings.HasPrefix(p, "/") {
			return fmt.Errorf("rejected: %s", p)
		}
		return nil
	}
	tools := BindEnv(env.Open("env-1", nil).WithFiles(files), FileTools())

	_, err := executeNamed(t, tools, "read", `{"path":".."}`)
	if err == nil || !strings.Contains(err.Error(), "..") {
		t.Fatalf("read .. error = %v, want rejected path", err)
	}
	_, err = executeNamed(t, tools, "write", `{"path":"/tmp/x","content":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "/tmp/x") {
		t.Fatalf("write abs error = %v, want rejected path", err)
	}
}

type fakeFiles struct {
	mu     sync.Mutex
	data   map[string][]byte
	reject func(path string) error
}

func newFakeFiles(files map[string]string) *fakeFiles {
	data := make(map[string][]byte, len(files))
	for name, content := range files {
		data[name] = []byte(content)
	}
	return &fakeFiles{data: data}
}

func (f *fakeFiles) denied(p string) error {
	if f.reject != nil {
		return f.reject(p)
	}
	return nil
}

func (f *fakeFiles) Read(p string) ([]byte, error) {
	if err := f.denied(p); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.data[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

func (f *fakeFiles) Write(p string, data []byte) error {
	if err := f.denied(p); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data == nil {
		f.data = map[string][]byte{}
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	f.data[p] = cp
	return nil
}

func (f *fakeFiles) Delete(p string) error {
	if err := f.denied(p); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, p)
	return nil
}

func (f *fakeFiles) Stat(p string) (env.FileInfo, error) {
	if err := f.denied(p); err != nil {
		return env.FileInfo{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if data, ok := f.data[p]; ok {
		return env.FileInfo{Name: path.Base(p), Size: int64(len(data))}, nil
	}
	if f.hasChildren(p) {
		return env.FileInfo{Name: path.Base(p), IsDir: true}, nil
	}
	return env.FileInfo{}, fmt.Errorf("not found: %s", p)
}

func (f *fakeFiles) List(dir string) ([]env.DirEnt, error) {
	if err := f.denied(dir); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if dir == "" {
		dir = "."
	}
	if _, ok := f.data[dir]; ok {
		return nil, fmt.Errorf("not a directory: %s", dir)
	}
	children := map[string]bool{}
	prefix := ""
	if dir != "." {
		prefix = strings.TrimSuffix(dir, "/") + "/"
	}
	for name := range f.data {
		rest := name
		if prefix != "" {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			rest = name[len(prefix):]
		}
		entry, more, found := strings.Cut(rest, "/")
		if entry == "" {
			continue
		}
		if found {
			children[entry] = true
			_ = more
			continue
		}
		if _, exists := children[entry]; !exists {
			children[entry] = false
		}
	}
	if len(children) == 0 && dir != "." {
		return nil, fmt.Errorf("not found: %s", dir)
	}
	out := make([]env.DirEnt, 0, len(children))
	for name, isDir := range children {
		out = append(out, env.DirEnt{Name: name, IsDir: isDir})
	}
	return out, nil
}

func (f *fakeFiles) hasChildren(dir string) bool {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	if dir == "." || dir == "" {
		return len(f.data) > 0
	}
	for name := range f.data {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
