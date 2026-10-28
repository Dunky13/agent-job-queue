package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func identityForTest(t *testing.T, argv []string, cwd string, env map[string]string) Object {
	t.Helper()
	result, e := IdentifyWorkload(argv, cwd, env)
	if e != nil {
		t.Fatal(e)
	}
	return result
}
func TestWorkloadIdentityWorkersVariantsArgumentsAndCaches(t *testing.T) {
	directory := t.TempDir()
	first := identityForTest(t, []string{"go", "test", "-p", "2", "./..."}, directory, map[string]string{"PATH": "/nonexistent"})
	second := identityForTest(t, []string{"go", "test", "-p4", "./..."}, directory, map[string]string{"PATH": "/nonexistent"})
	if first["family"] != second["family"] || first["variant"] == second["variant"] {
		t.Fatal("Worker flags changed family or failed to separate variants")
	}
	target := identityForTest(t, []string{"go", "test", "-p", "2", "./different"}, directory, map[string]string{"PATH": "/nonexistent"})
	if target["family"] == first["family"] {
		t.Fatal("Different targets merged")
	}
	unknown := identityForTest(t, []string{"go", "test", "-p", "2", "./...", "--unknown"}, directory, map[string]string{"PATH": "/nonexistent"})
	if unknown["family"] == first["family"] {
		t.Fatal("Unknown semantic argument discarded")
	}
	os.Mkdir(filepath.Join(directory, "build"), 0700)
	cached := identityForTest(t, []string{"go", "test", "-p", "2", "./..."}, directory, map[string]string{"PATH": "/nonexistent"})
	if cached["family"] != first["family"] || cached["variant"] != first["variant"] || cached["context"] == first["context"] {
		t.Fatal("Cache context leaked into workload family/variant")
	}
	changedEnv := identityForTest(t, []string{"go", "test", "-p", "2", "./..."}, directory, map[string]string{"PATH": "/nonexistent", "CUSTOM": "semantic"})
	if changedEnv["family"] != first["family"] || changedEnv["variant"] == first["variant"] {
		t.Fatal("Semantic environment not separated")
	}
}
func TestWorkloadIdentityInlineProgramsNeverProbeCodeOrPositionalData(t *testing.T) {
	directory := t.TempDir()
	code := "some-inline-program"
	if e := os.WriteFile(filepath.Join(directory, code), []byte("first"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, argv := range [][]string{{"python3", "-c", code, "data"}, {"uv", "run", "python3", "-Iqc", code, "data"}, {"bash", "-lc", code, "data"}, {"node", "--eval=" + code, "data"}, {"env", "node", "-e", code, "data"}} {
		first := identityForTest(t, argv, directory, map[string]string{"PATH": "/nonexistent"})
		os.WriteFile(filepath.Join(directory, code), []byte("second"), 0600)
		os.WriteFile(filepath.Join(directory, "data"), []byte("changed positional data"), 0600)
		second := identityForTest(t, argv, directory, map[string]string{"PATH": "/nonexistent"})
		if first["variant"] != second["variant"] {
			t.Fatalf("Inline code/data inspected as files: %v", argv)
		}
		os.Remove(filepath.Join(directory, "data"))
	}
	first := identityForTest(t, []string{"bash", "-c", "echo 1", "-j", "2"}, directory, map[string]string{"PATH": "/nonexistent"})
	second := identityForTest(t, []string{"bash", "-c", "echo 1", "-j", "4"}, directory, map[string]string{"PATH": "/nonexistent"})
	if first["family"] == second["family"] {
		t.Fatal("Opaque shell positional arguments normalized as worker counts")
	}
}
func TestWorkloadIdentityOptionalProvenanceAndExplicitGrouping(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "script.sh")
	os.WriteFile(script, []byte("echo first"), 0600)
	first := identityForTest(t, []string{"sh", "script.sh"}, directory, map[string]string{"PATH": "/nonexistent"})
	os.WriteFile(script, []byte("echo second"), 0600)
	second := identityForTest(t, []string{"sh", "script.sh"}, directory, map[string]string{"PATH": "/nonexistent"})
	if first["family"] != second["family"] || first["variant"] == second["variant"] {
		t.Fatal("Script content did not separate variants")
	}
	for _, arg := range []string{strings.Repeat("x", 4096), "script.sh/not-a-directory"} {
		identityForTest(t, []string{"unknown", arg}, directory, map[string]string{"PATH": "/nonexistent"})
	}
	for _, profile := range []string{"", " ", strings.Repeat("x", 257)} {
		if _, e := IdentifyWorkload([]string{"unknown"}, directory, map[string]string{"AGENT_HEAVY_PROFILE": profile}); e == nil {
			t.Fatal("Invalid explicit profile accepted")
		}
	}
	a := identityForTest(t, []string{"sh", "-c", "different first"}, directory, map[string]string{"PATH": "/nonexistent", "AGENT_HEAVY_PROFILE": "deliberate"})
	b := identityForTest(t, []string{"sh", "-c", "different second"}, directory, map[string]string{"PATH": "/nonexistent", "AGENT_HEAVY_PROFILE": "deliberate"})
	if a["family"] != b["family"] || a["variant"] == b["variant"] {
		t.Fatal("Explicit profile failed to group opaque commands")
	}
	if _, e := IdentifyWorkload([]string{"bad\x00arg"}, directory, nil); e == nil {
		t.Fatal("NUL command accepted")
	}
}
func TestWorkloadIdentityLinkedCheckoutPathsAndToolReplacement(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	common := filepath.Join(root, "common")
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, p := range []string{bin, common, first, second} {
		os.Mkdir(p, 0700)
	}
	git := `#!/bin/sh
case "$4" in
 --path-format=absolute) printf '%s\n%s\n' "$2" "` + common + `" ;;
 HEAD) printf '%s\n' revision-fixture ;;
 *) exit 1 ;;
esac
`
	if e := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin)
	for _, p := range []string{first, second} {
		os.WriteFile(filepath.Join(p, "package.json"), []byte(`{"scripts":{"test":"fixture"}}`), 0600)
	}
	a := identityForTest(t, []string{"npm", "test", "--prefix=" + first}, first, map[string]string{"PATH": "/nonexistent", "CUSTOM": first + "/file"})
	b := identityForTest(t, []string{"npm", "test", "--prefix=" + second}, second, map[string]string{"PATH": "/nonexistent", "CUSTOM": second + "/file"})
	if profileDirectoryScope(first) != profileDirectoryScope(second) {
		t.Fatal("Linked worktrees do not share a learning scope")
	}
	if a["family"] != b["family"] || a["variant"] != b["variant"] {
		t.Fatalf("Linked checkout names leaked into identity: %v %v", a, b)
	}
	os.WriteFile(filepath.Join(second, "package.json"), []byte(`{"scripts":{"test":"changed"}}`), 0600)
	changed := identityForTest(t, []string{"npm", "test", "--prefix=" + second}, second, map[string]string{"PATH": "/nonexistent", "CUSTOM": second + "/file"})
	if changed["family"] != a["family"] || changed["variant"] == a["variant"] {
		t.Fatal("Definition change did not keep family/separate variant")
	}
	executable := filepath.Join(bin, "tool")
	os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700)
	a = identityForTest(t, []string{executable, "test"}, first, map[string]string{"PATH": bin})
	os.WriteFile(executable, []byte("#!/bin/sh\nexit 100\n"), 0700)
	b = identityForTest(t, []string{executable, "test"}, first, map[string]string{"PATH": bin})
	if a["family"] != b["family"] || a["variant"] == b["variant"] {
		t.Fatal("Toolchain replacement not detected")
	}
}

func TestWorkloadIdentityIgnoresAgentInvocationMetadata(t *testing.T) {
	directory := t.TempDir()
	first := identityForTest(t, []string{"go", "test", "./..."}, directory, map[string]string{
		"PATH": "/nonexistent", "CODEX_SESSION_ID": "session-a", "CODEX_THREAD_ID": "thread-a",
		"CODEX_TOOL_CALL_ID": "call-a", "SSH_AUTH_SOCK": "/private/socket-a", "GOMAXPROCS": "2",
	})
	second := identityForTest(t, []string{"go", "test", "./..."}, directory, map[string]string{
		"PATH": "/nonexistent", "CODEX_SESSION_ID": "session-b", "CODEX_THREAD_ID": "thread-b",
		"CODEX_TOOL_CALL_ID": "call-b", "SSH_AUTH_SOCK": "/private/socket-b", "GOMAXPROCS": "2",
	})
	if first["family"] != second["family"] || first["variant"] != second["variant"] {
		t.Fatal("Agent invocation metadata prevents repeated workload learning")
	}
	resourceChange := identityForTest(t, []string{"go", "test", "./..."}, directory, map[string]string{
		"PATH": "/nonexistent", "GOMAXPROCS": "8",
	})
	if resourceChange["variant"] == second["variant"] {
		t.Fatal("Resource controls were discarded with invocation metadata")
	}
}
