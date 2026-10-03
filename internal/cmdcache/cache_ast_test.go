package cmdcache_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/cmdcache"
)

func astCache(t *testing.T) (*cmdcache.Cache, string) {
	t.Helper()
	cache, err := cmdcache.New(cmdcache.Config{Dir: t.TempDir(), CacheFailures: true})
	if err != nil {
		t.Fatal(err)
	}
	return cache, t.TempDir()
}

func TestCacheASTKeyReusesStaticFormattingAndSafeQuotes(t *testing.T) {
	tests := []struct {
		name     string
		commands []string
	}{
		{
			name:     "ordinary literal",
			commands: []string{"echo foo", "  echo\t'foo'  ", "echo \"foo\"", "echo foo # ignored comment"},
		},
		{
			name:     "pipeline and redirection",
			commands: []string{"cat 'input.txt' 2>&1 | head -1", "cat input.txt  2>&1|head   -1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, source := range tt.commands {
				t.Run(source, func(t *testing.T) {
					cache, live := astCache(t)
					key := cmdcache.Key{Command: source, Backend: "external", EnvHash: "env"}
					entry, err := cache.Store(
						live,
						key,
						cmdcache.Observation{},
						cmdcache.Result{Output: "stored result"},
					)
					if err != nil || entry == nil || entry.Command != source {
						t.Fatalf("Store did not retain the original command: %+v, %v", entry, err)
					}
					for _, request := range tt.commands {
						key.Command = request
						before := cache.Stats()
						peek, err := cache.Peek(live, key)
						if err != nil || peek == nil || peek.Output != "stored result" {
							t.Fatalf(
								"Peek(%q) did not reuse %q: %+v, %v",
								request,
								source,
								peek,
								err,
							)
						}
						if !reflect.DeepEqual(before, cache.Stats()) {
							t.Fatal("Peek changed statistics")
						}
						hit, err := cache.Lookup(live, key)
						if err != nil || hit == nil || hit.Output != "stored result" || hit.Command != source {
							t.Fatalf(
								"Lookup(%q) did not reuse %q: %+v, %v",
								request,
								source,
								hit,
								err,
							)
						}
					}
				})
			}
		})
	}
}

func TestCacheASTKeyPreservesMeaningfulQuotes(t *testing.T) {
	tests := []struct{ name, stored, request string }{
		{name: "glob", stored: "echo '*.go'", request: "echo *.go"},
		{name: "parameter", stored: "echo '$NAME'", request: "echo $NAME"},
		{name: "tilde", stored: "echo '~'", request: "echo ~"},
		{name: "brace expansion", stored: "echo '{a,b}'", request: "echo {a,b}"},
		{name: "word boundary", stored: "echo 'a b'", request: "echo a b"},
		{name: "empty argument", stored: "printf '%s|' ''", request: "printf '%s|'"},
		{name: "escape sequence", stored: "echo 'a\\n'", request: "echo $'a\\n'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, live := astCache(t)
			key := cmdcache.Key{Command: tt.stored, Backend: "external", EnvHash: "env"}
			entry, err := cache.Store(
				live,
				key,
				cmdcache.Observation{},
				cmdcache.Result{},
			)
			if err != nil || entry == nil {
				t.Fatalf("Store: %+v, %v", entry, err)
			}
			key.Command = tt.request
			if hit, err := cache.Lookup(live, key); err != nil || hit != nil {
				t.Fatalf(
					"different shell meaning reused %q for %q: %+v, %v",
					tt.stored,
					tt.request,
					hit,
					err,
				)
			}
			if peek, err := cache.Peek(live, key); err != nil || peek != nil {
				t.Fatalf("Peek merged different shell meaning: %+v, %v", peek, err)
			}
		})
	}
}

func TestCacheASTKeyFallsBackForObservableShellSource(t *testing.T) {
	tests := []struct{ name, stored, request string }{
		{name: "line number", stored: `printf '%s' "$LINENO"`, request: "\nprintf '%s' \"$LINENO\""},
		{name: "current command", stored: `printf '%s' "$BASH_COMMAND"`, request: `printf   '%s' "$BASH_COMMAND"`},
		{
			name:    "execution string",
			stored:  `printf '%s' "$BASH_EXECUTION_STRING"`,
			request: `printf   '%s' "$BASH_EXECUTION_STRING"`,
		},
		{
			name:    "debug trap",
			stored:  `trap 'echo "$BASH_COMMAND"' DEBUG; echo foo`,
			request: `trap  'echo "$BASH_COMMAND"' DEBUG; echo foo`,
		},
		{name: "eval", stored: "eval 'echo foo'", request: "eval  'echo foo'"},
		{name: "builtin eval", stored: "builtin eval 'echo foo'", request: "builtin  eval 'echo foo'"},
		{name: "source", stored: "source ./setup.sh", request: "source  ./setup.sh"},
		{name: "shell variables", stored: "set", request: "  set"},
		{
			name:    "quoted declaration reads static variable name",
			stored:  `'declare' -p BASH_EXECUTION_STRING`,
			request: `"declare" -p BASH_EXECUTION_STRING`,
		},
		{
			name:    "quoted declaration source spacing",
			stored:  `'declare' -p BASH_EXECUTION_STRING`,
			request: `'declare'  -p BASH_EXECUTION_STRING`,
		},
		{
			name:    "quoted typeset reads static variable name",
			stored:  `'typeset' -p BASH_EXECUTION_STRING`,
			request: `'typeset'  -p BASH_EXECUTION_STRING`,
		},
		{name: "quoted arithmetic builtin", stored: `'let' LINENO`, request: `'let'  LINENO`},
		{
			name:    "printf variable target arithmetic",
			stored:  `printf -v 'a[BASH_EXECUTION_STRING]' '%s' x`,
			request: `printf  '-v' 'a[BASH_EXECUTION_STRING]' '%s' x`,
		},
		{
			name:    "printf compound quoted variable option",
			stored:  `printf -'v' 'a[BASH_EXECUTION_STRING]' '%s' x`,
			request: `printf  -'v' 'a[BASH_EXECUTION_STRING]' '%s' x`,
		},
		{
			name:    "printf ANSI quoted variable option",
			stored:  `printf $'-v' 'a[BASH_EXECUTION_STRING]' '%s' x`,
			request: `printf  $'-v' 'a[BASH_EXECUTION_STRING]' '%s' x`,
		},
		{
			name:    "printf quoted attached variable target",
			stored:  `printf '-va[BASH_EXECUTION_STRING]' '%s' x`,
			request: `printf  '-va[BASH_EXECUTION_STRING]' '%s' x`,
		},
		{
			name:    "read variable target",
			stored:  `read -r 'a[BASH_EXECUTION_STRING]' < input.txt`,
			request: `read  -r 'a[BASH_EXECUTION_STRING]' < input.txt`,
		},
		{
			name:    "unset array element",
			stored:  `unset 'a[BASH_EXECUTION_STRING]'`,
			request: `unset  'a[BASH_EXECUTION_STRING]'`,
		},
		{
			name:    "mapfile variable target",
			stored:  `mapfile a < input.txt`,
			request: `mapfile 'a' < input.txt`,
		},
		{
			name:    "readarray variable target",
			stored:  `readarray a < input.txt`,
			request: `readarray 'a' < input.txt`,
		},
		{
			name:    "getopts variable target",
			stored:  `getopts a 'a[BASH_EXECUTION_STRING]' -a`,
			request: `getopts  a 'a[BASH_EXECUTION_STRING]' -a`,
		},
		{
			name:    "test variable target",
			stored:  `test -v 'a[BASH_EXECUTION_STRING]'`,
			request: `test '-v' 'a[BASH_EXECUTION_STRING]'`,
		},
		{
			name:    "test compound quoted variable option",
			stored:  `test -'v' 'a[BASH_EXECUTION_STRING]'`,
			request: `test  -'v' 'a[BASH_EXECUTION_STRING]'`,
		},
		{
			name:    "bracket variable target",
			stored:  `[ -v 'a[BASH_EXECUTION_STRING]' ]`,
			request: `[  -v 'a[BASH_EXECUTION_STRING]' ]`,
		},
		{name: "dynamic command", stored: `"$runner" foo`, request: `"$runner"  foo`},
		{name: "arithmetic", stored: `printf '%s' "$((LINENO))"`, request: `printf  '%s' "$((LINENO))"`},
		{name: "multiline error location", stored: "printf foo\nnot-a-command", request: "printf foo; not-a-command"},
		{name: "syntax error", stored: "printf 'unterminated", request: "printf  'unterminated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, live := astCache(t)
			key := cmdcache.Key{Command: tt.stored, Backend: "external", EnvHash: "env"}
			entry, err := cache.Store(
				live,
				key,
				cmdcache.Observation{},
				cmdcache.Result{},
			)
			if err != nil || entry == nil {
				t.Fatalf("Store: %+v, %v", entry, err)
			}
			key.Command = tt.request
			if hit, err := cache.Lookup(live, key); err != nil || hit != nil {
				t.Fatalf("observable-source variant reused raw entry: %+v, %v", hit, err)
			}
			if peek, err := cache.Peek(live, key); err != nil || peek != nil {
				t.Fatalf("Peek merged observable-source variants: %+v, %v", peek, err)
			}
			key.Command = tt.stored
			if hit, err := cache.Lookup(live, key); err != nil || hit == nil {
				t.Fatalf("raw fallback lost exact-command reuse: %+v, %v", hit, err)
			}
		})
	}
}

func TestCacheASTKeyKeepsBackendEnvironmentAndDependencies(t *testing.T) {
	tests := []struct {
		name, backend, environment, content, role string
		wantHit                                   bool
	}{
		{
			name: "cross role", backend: "external", environment: "env",
			content: "one", role: "verifier", wantHit: true,
		},
		{name: "other backend", backend: "bwrap", environment: "env", content: "one"},
		{name: "other environment", backend: "external", environment: "other", content: "one"},
		{name: "changed dependency", backend: "external", environment: "env", content: "two"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, live := astCache(t)
			input := filepath.Join(live, "input.txt")
			if err := os.WriteFile(input, []byte("one"), 0o600); err != nil {
				t.Fatal(err)
			}
			key := cmdcache.Key{Command: "cat 'input.txt'", Backend: "external", EnvHash: "env", Role: "executor"}
			obs := cmdcache.Observation{Reads: map[string]cmdcache.ReadKind{"input.txt": cmdcache.ReadFile}}
			entry, err := cache.Store(
				live,
				key,
				obs,
				cmdcache.Result{Output: "one"},
			)
			if err != nil || entry == nil {
				t.Fatalf("Store: %+v, %v", entry, err)
			}
			if err := os.WriteFile(input, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			key.Command, key.Backend, key.EnvHash, key.Role = "cat input.txt", tt.backend, tt.environment, tt.role
			hit, err := cache.Lookup(live, key)
			if err != nil || (hit != nil) != tt.wantHit {
				t.Fatalf(
					"Lookup = %+v, %v, want hit %v",
					hit,
					err,
					tt.wantHit,
				)
			}
		})
	}
}

func TestCacheASTKeyDoesNotReadRawVersionThreeEntries(t *testing.T) {
	dir, live := t.TempDir(), t.TempDir()
	// Fixture for the previous on-disk format, independent of the new index code.
	oldHash := sha256.Sum256([]byte("tmcmd3\ncat input.txt\nexternal\nenv"))
	oldDir := filepath.Join(dir, "index", fmt.Sprintf("%x", oldHash))
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"command":"cat input.txt","backend":"external","env_hash":"env","reads":{},"output":"legacy result"}`
	if err := os.WriteFile(filepath.Join(oldDir, "legacy.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, err := cmdcache.New(cmdcache.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	key := cmdcache.Key{Command: "cat input.txt", Backend: "external", EnvHash: "env"}
	if hit, err := cache.Lookup(live, key); err != nil || hit != nil {
		t.Fatalf("version-three entry reused: %+v, %v", hit, err)
	}
	if peek, err := cache.Peek(live, key); err != nil || peek != nil {
		t.Fatalf("Peek read version-three entry: %+v, %v", peek, err)
	}
}

func TestCacheASTKeyInvalidatesAcrossStaticSpellings(t *testing.T) {
	cache, live := astCache(t)
	key := cmdcache.Key{Command: "echo 'foo'", Backend: "external"}
	entry, err := cache.Store(
		live,
		key,
		cmdcache.Observation{},
		cmdcache.Result{},
	)
	if err != nil || entry == nil {
		t.Fatalf("Store: %+v, %v", entry, err)
	}
	key.Command = "echo foo"
	if err := cache.Invalidate(key, entry); err != nil {
		t.Fatal(err)
	}
	key.Command = "echo 'foo'"
	if hit, err := cache.Lookup(live, key); err != nil || hit != nil {
		t.Fatalf("Invalidate left an equivalent spelling reusable: %+v, %v", hit, err)
	}
}
