package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeVFSConfig(t *testing.T, liveRoot string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.yaml")
	content := "llm:\n  provider: openai-responses\n  base_url: https://api.openai.com/v1\n  credential: test\n  model: gpt-5\nvfs:\n  live_root: " + liveRoot + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVFSLiveRootAcceptsAbsolutePath(t *testing.T) {
	got, err := LoadConfigFile(writeVFSConfig(t, "/mnt/threadmill-live"))
	if err != nil {
		t.Fatal(err)
	}
	if got.VFS.LiveRoot != "/mnt/threadmill-live" {
		t.Fatalf("live_root = %q", got.VFS.LiveRoot)
	}
}

func TestVFSLiveRootRejectsRelativePath(t *testing.T) {
	_, err := LoadConfigFile(writeVFSConfig(t, "relative/live"))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want absolute-path rejection", err)
	}
}

func TestSoftMemoryLimitRejectsNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	content := []byte("llm:\n  provider: openai-responses\n  base_url: https://api.openai.com/v1\n  credential: test\n  model: gpt-5\nmemory:\n  soft_memory_limit_mb: -1\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfigFile(path)
	if err == nil || !strings.Contains(err.Error(), "soft_memory_limit_mb") {
		t.Fatalf("err = %v, want soft_memory_limit_mb rejection", err)
	}
}
