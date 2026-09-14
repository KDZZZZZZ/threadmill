package cmdcache

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel, data string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func TestPathStateReportsAbsent(t *testing.T) {
	root := t.TempDir()
	state, err := PathState(root, "missing.txt")
	if err != nil {
		t.Fatal(err)
	}
	if state != stateAbsent {
		t.Fatalf("state = %q, want %q", state, stateAbsent)
	}
}

// 同内容同权限必须得到同状态串，否则跨 agent 永远无法命中。
func TestPathStateStableAcrossEnvironments(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeFile(t, a, "go.mod", "module x\n", 0o644)
	writeFile(t, b, "go.mod", "module x\n", 0o644)
	stateA, err := PathState(a, "go.mod")
	if err != nil {
		t.Fatal(err)
	}
	stateB, err := PathState(b, "go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if stateA != stateB {
		t.Fatalf("state differs across environments: %q vs %q", stateA, stateB)
	}
}

func TestPathStateChangesWithContent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "one", 0o644)
	before, err := PathState(root, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "a.txt", "two", 0o644)
	after, err := PathState(root, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("state should change with content")
	}
}

// 可执行位是构建产物的语义一部分，必须进状态串。
func TestPathStateChangesWithExecutableBit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "run.sh", "#!/bin/sh\n", 0o644)
	plain, err := PathState(root, "run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := PathState(root, "run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if plain == executable {
		t.Fatal("state should change with the executable bit")
	}
}

// 目录依赖比对条目名集合，不比对条目内容：./... 展开关心的是有哪些包，
// 不是每个包里写了什么。
func TestDirStateTracksEntryNamesNotContents(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/a.go", "package a", 0o644)
	before, err := PathState(root, "pkg")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "pkg/a.go", "package a // edited", 0o644)
	sameNames, err := PathState(root, "pkg")
	if err != nil {
		t.Fatal(err)
	}
	if before != sameNames {
		t.Fatal("directory state must not depend on file contents")
	}
	writeFile(t, root, "pkg/b.go", "package a", 0o644)
	added, err := PathState(root, "pkg")
	if err != nil {
		t.Fatal(err)
	}
	if before == added {
		t.Fatal("directory state must change when an entry appears")
	}
}

// 文件被替换成同名目录必须表现为状态变化。
func TestPathStateDistinguishesFileFromDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "x", "data", 0o644)
	asFile, err := PathState(root, "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	asDir, err := PathState(root, "x")
	if err != nil {
		t.Fatal(err)
	}
	if asFile == asDir {
		t.Fatal("file and directory must not share a state")
	}
}

// 逃出工作区的路径一律拒绝：被污染的缓存条目不能借回放写到树外。
func TestPathStateRejectsEscapingPath(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../outside", "a/../../outside", "/abs"} {
		if _, err := PathState(root, rel); err == nil {
			t.Fatalf("PathState(%q) should have failed", rel)
		}
	}
}

// 符号链接按目标解析，不跟随：跟随会把树外内容读进状态串。
func TestPathStateHashesSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	state, err := PathState(root, "link")
	if err != nil {
		t.Fatal(err)
	}
	if state == stateAbsent {
		t.Fatal("symlink should have a state")
	}
	if err := os.Remove(filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	retargeted, err := PathState(root, "link")
	if err != nil {
		t.Fatal(err)
	}
	if state == retargeted {
		t.Fatal("state should change with the symlink target")
	}
}

// 命令自己产出的名字要在目录状态里被排除，否则「执行后算出的目录状态」
// 不等于执行前，第一次缓存就永远匹配不上全新环境。
func TestDirStateExcludesManagedNames(t *testing.T) {
	fresh, built := t.TempDir(), t.TempDir()
	writeFile(t, fresh, "main.go", "package main", 0o644)
	writeFile(t, built, "main.go", "package main", 0o644)
	writeFile(t, built, "app", "binary", 0o755)

	managed := map[string]struct{}{"app": {}}
	freshState, err := pathStateExcluding(fresh, ".", managed)
	if err != nil {
		t.Fatal(err)
	}
	builtState, err := pathStateExcluding(built, ".", managed)
	if err != nil {
		t.Fatal(err)
	}
	if freshState != builtState {
		t.Fatal("directory state must ignore names the command produces")
	}
	if plain, err := PathState(built, "."); err != nil {
		t.Fatal(err)
	} else if plain == freshState {
		t.Fatal("without the exclusion the two directories must differ")
	}
}

// 排除只对指定名字生效，无关新增仍然要让目录状态变化。
func TestDirStateStillTracksUnmanagedNames(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main", 0o644)
	managed := map[string]struct{}{"app": {}}
	before, err := pathStateExcluding(root, ".", managed)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "other.go", "package main", 0o644)
	after, err := pathStateExcluding(root, ".", managed)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("an unmanaged new entry must change the directory state")
	}
}
