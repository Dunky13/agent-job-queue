package tooling

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const goRuntimeModule = "github.com/Dunky13/agent-job-queue"

func goRuntimeFixture(t *testing.T, packages string, listStatus, testStatus string, selection ...string) (string, error, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "packages"), []byte(packages), 0600); err != nil {
		t.Fatal(err)
	}
	testTool(t, root, "go", `case "$1" in
list)
  pwd > "$GO_RUNTIME_TEST_ROOT/cwd"
  printf '%s\0' "$@" >> "$GO_RUNTIME_TEST_ROOT/list-args"
  cat "$GO_RUNTIME_TEST_ROOT/packages"
  [ "$GO_RUNTIME_LIST_STATUS" = 0 ] || printf 'fixture enumeration failed\n' >&2
  exit "$GO_RUNTIME_LIST_STATUS";;
test)
  printf '%s\0' "$@" >> "$GO_RUNTIME_TEST_ROOT/test-args"
  exit "$GO_RUNTIME_TEST_STATUS";;
*) exit 99;;
esac`)
	script, err := filepath.Abs("../../scripts/test-go-runtime.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", append([]string{script}, selection...)...)
	// Invocation from elsewhere must still enumerate the repository's packages.
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GO_RUNTIME_TEST_ROOT="+root, "GO_RUNTIME_LIST_STATUS="+listStatus, "GO_RUNTIME_TEST_STATUS="+testStatus)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("runtime script did not finish: %v: %s", ctx.Err(), output)
	}
	return root, err, string(output)
}

func goRuntimeArguments(t *testing.T, root, name string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, name+"-args"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\x00"), "\x00")
}

func TestGoRuntimePartitionsCompleteScopeWithExactFlags(t *testing.T) {
	// New packages and tooling children must remain in the runtime partition.
	packages := []string{
		goRuntimeModule + "/cmd/new-command",
		goRuntimeModule + "/internal/backend",
		goRuntimeModule + "/internal/tooling",
		goRuntimeModule + "/internal/tooling/child",
		goRuntimeModule + "/internal/tooling-extra",
		goRuntimeModule + "/internal/new-package",
		goRuntimeModule + "/packaging/homebrew",
		goRuntimeModule + "/remote/static",
		goRuntimeModule + "/cmd",
		goRuntimeModule + "/internal",
		goRuntimeModule + "/internal/remote",
		goRuntimeModule + "/internal/remote/child",
		goRuntimeModule + "/internal/remote-extra",
	}
	root, err, output := goRuntimeFixture(t, strings.Join(packages, "\n")+"\n", "0", "0")
	if err != nil {
		t.Fatalf("runtime suite: %v: %s", err, output)
	}
	if got, want := goRuntimeArguments(t, root, "list"), []string{"list", "-p", "2", "./cmd/...", "./internal/...", "./packaging/homebrew", "./remote/static"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enumeration scope: got %q, want %q", got, want)
	}
	cwd, err := os.ReadFile(filepath.Join(root, "cwd"))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(cwd)) != repository {
		t.Fatalf("enumerated in %q, want repository %q", cwd, repository)
	}
	want := []string{"test", "-p", "2", "-parallel", "2", "-race"}
	for _, index := range []int{0, 1, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12} {
		want = append(want, packages[index])
	}
	want = append(want, "-timeout", "12m")
	if got := goRuntimeArguments(t, root, "test"); !reflect.DeepEqual(got, want) {
		t.Fatalf("complete unique runtime partition and test flags: got %q, want %q", got, want)
	}
}

func TestGoRuntimeCoreAndHTTPSCoverOriginalScopeExactlyOnce(t *testing.T) {
	packages := []string{
		goRuntimeModule + "/internal/tooling", goRuntimeModule + "/internal/backend",
		goRuntimeModule + "/internal/remote", goRuntimeModule + "/internal/remote/child",
		goRuntimeModule + "/internal/remote-extra", goRuntimeModule + "/cmd/new-command",
	}
	selected := map[string]int{}
	for _, suite := range []struct {
		name string
		want []string
	}{
		{"core", []string{packages[1], packages[3], packages[4], packages[5]}},
		{"https", []string{packages[2]}},
	} {
		root, err, output := goRuntimeFixture(t, strings.Join(packages, "\n"), "0", "0", suite.name)
		if err != nil {
			t.Fatalf("%s: %v: %s", suite.name, err, output)
		}
		want := append([]string{"test", "-p", "2", "-parallel", "2", "-race"}, suite.want...)
		want = append(want, "-timeout", "12m")
		if got := goRuntimeArguments(t, root, "test"); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed scope or flags: got %q, want %q", suite.name, got, want)
		}
		for _, name := range suite.want {
			selected[name]++
		}
	}
	for _, name := range packages[1:] {
		if selected[name] != 1 {
			t.Fatal("original runtime scope omitted or duplicated", name, selected[name])
		}
	}
}

func TestGoRuntimeRejectsInvalidSelectionBeforeEnumeration(t *testing.T) {
	for _, selection := range [][]string{{""}, {"unknown"}, {"--run=Nothing"}, {"core", "https"}} {
		root, err, output := goRuntimeFixture(t, "unused", "0", "0", selection...)
		if err == nil || !strings.Contains(output, "Usage:") {
			t.Fatalf("invalid selection accepted: %q: %v: %s", selection, err, output)
		}
		for _, file := range []string{"list-args", "test-args"} {
			if _, err := os.Stat(filepath.Join(root, file)); !os.IsNotExist(err) {
				t.Fatal("invalid selection reached Go", selection, file, err)
			}
		}
	}
}

func TestGoRuntimeRejectsIncompleteSelectedScope(t *testing.T) {
	for _, suite := range []struct{ name, packages string }{
		{"core", goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/remote"},
		{"https", goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/backend"},
	} {
		root, err, output := goRuntimeFixture(t, suite.packages, "0", "0", suite.name)
		if err == nil || !strings.Contains(output, "nonempty runtime suite") {
			t.Fatalf("incomplete %s accepted: %v: %s", suite.name, err, output)
		}
		if _, err := os.Stat(filepath.Join(root, "test-args")); !os.IsNotExist(err) {
			t.Fatal("incomplete partition reached tests", suite.name, err)
		}
	}
}

func TestGoRuntimeFailsBeforeTestingInvalidEnumeration(t *testing.T) {
	runtime := goRuntimeModule + "/internal/backend"
	tooling := goRuntimeModule + "/internal/tooling"
	https := goRuntimeModule + "/internal/remote"
	valid := runtime + "\n" + tooling + "\n" + https + "\n"
	for _, test := range []struct {
		name, packages, status, diagnostic string
	}{
		{"enumeration error", "", "23", "fixture enumeration failed"},
		{"partial enumeration error", valid, "23", "fixture enumeration failed"},
		{"empty", "", "0", "Unexpected Go package"},
		{"missing tooling", runtime, "0", "requires tooling"},
		{"missing HTTPS", runtime + "\n" + tooling, "0", "requires tooling and HTTPS"},
		{"empty runtime", tooling, "0", "nonempty runtime suite"},
		{"duplicate runtime", valid + runtime, "0", "Duplicate Go package"},
		{"duplicate tooling", valid + tooling, "0", "Duplicate Go package"},
		{"duplicate HTTPS", valid + https, "0", "Duplicate Go package"},
		{"blank record", runtime + "\n\n" + tooling, "0", "Unexpected Go package"},
		{"option", valid + "-run=Nothing", "0", "Unexpected Go package"},
		{"foreign module", valid + "example.com/other/internal/tooling", "0", "Unexpected Go package"},
		{"outside scope", valid + goRuntimeModule + "/other", "0", "Unexpected Go package"},
		{"prefix confusion", valid + goRuntimeModule + "/internal-extra/pkg", "0", "Unexpected Go package"},
		{"whitespace", valid + runtime + " -run=Nothing", "0", "Unsafe Go package"},
		{"carriage return", valid + runtime + "\r", "0", "Unsafe Go package"},
		{"glob", valid + goRuntimeModule + "/internal/*", "0", "Unsafe Go package"},
		{"pattern", valid + goRuntimeModule + "/internal/...", "0", "Unsafe Go package"},
		{"parent segment", valid + goRuntimeModule + "/internal/../tooling", "0", "Unsafe Go package"},
		{"dot segment", valid + goRuntimeModule + "/internal/./tooling", "0", "Unsafe Go package"},
		{"empty segment", valid + goRuntimeModule + "/internal//tooling", "0", "Unsafe Go package"},
		{"trailing slash", valid + runtime + "/", "0", "Unsafe Go package"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err, output := goRuntimeFixture(t, test.packages, test.status, "0")
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !strings.Contains(output, test.diagnostic) {
				t.Fatalf("invalid enumeration was not refused: %v: %s", err, output)
			}
			if test.status != "0" && exit.ExitCode() != 23 {
				t.Fatalf("enumeration status lost: %d: %s", exit.ExitCode(), output)
			}
			if _, err := os.Stat(filepath.Join(root, "test-args")); !os.IsNotExist(err) {
				t.Fatalf("go test invoked after invalid enumeration: %v", err)
			}
		})
	}
}

func TestGoRuntimePreservesTestFailure(t *testing.T) {
	packages := goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/backend\n" + goRuntimeModule + "/internal/remote\n"
	for _, suite := range []string{"all", "core", "https"} {
		root, err, output := goRuntimeFixture(t, packages, "0", "37", suite)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 37 {
			t.Fatalf("%s go test failure lost: %v: %s", suite, err, output)
		}
		if got := goRuntimeArguments(t, root, "test"); len(got) == 0 || got[0] != "test" {
			t.Fatalf("%s runtime test was not invoked: %q", suite, got)
		}
	}
}
