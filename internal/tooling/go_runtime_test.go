package tooling

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const goRuntimeModule = "github.com/Dunky13/agent-job-queue"
const goRuntimeInstallerTest = "TestInstallerManagedLiveUpgradeRetainsRunningAttempts"
const goRuntimeInstallerPattern = "^" + goRuntimeInstallerTest + "$"
const goRuntimeInstallerSummary = "ok\t" + goRuntimeModule + "/internal/backend\t0.123s\n"

type goRuntimeFixtureConfig struct {
	packages        string
	listStatus      string
	testStatus      string
	inventory       string
	inventoryStatus string
	failTestCall    int
}

func goRuntimeDefaultConfig(packages, listStatus, testStatus string) goRuntimeFixtureConfig {
	return goRuntimeFixtureConfig{packages: packages, listStatus: listStatus, testStatus: testStatus,
		inventory: goRuntimeInstallerTest + "\n" + goRuntimeInstallerSummary, inventoryStatus: "0"}
}

func goRuntimeFixture(t *testing.T, packages string, listStatus, testStatus string, selection ...string) (string, error, string) {
	t.Helper()
	return goRuntimeConfiguredFixture(t, goRuntimeDefaultConfig(packages, listStatus, testStatus), selection...)
}

func goRuntimeConfiguredFixture(t *testing.T, config goRuntimeFixtureConfig, selection ...string) (string, error, string) {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{"packages": config.packages, "inventory": config.inventory, "test-count": "0\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	testTool(t, root, "go", `case "$1" in
list)
  printf 'packages\n' >> "$GO_RUNTIME_TEST_ROOT/actions"
  pwd > "$GO_RUNTIME_TEST_ROOT/cwd"
  printf '%s\0' "$@" >> "$GO_RUNTIME_TEST_ROOT/list-args"
  cat "$GO_RUNTIME_TEST_ROOT/packages"
  [ "$GO_RUNTIME_LIST_STATUS" = 0 ] || printf 'fixture enumeration failed\n' >&2
  exit "$GO_RUNTIME_LIST_STATUS";;
test)
  for argument in "$@"; do
    if [ "$argument" = -list ]; then
      printf 'inventory\n' >> "$GO_RUNTIME_TEST_ROOT/actions"
      printf '%s\0' "$@" >> "$GO_RUNTIME_TEST_ROOT/inventory-args"
      cat "$GO_RUNTIME_TEST_ROOT/inventory"
      [ "$GO_RUNTIME_INVENTORY_STATUS" = 0 ] || printf 'fixture inventory failed\n' >&2
      exit "$GO_RUNTIME_INVENTORY_STATUS"
    fi
  done
  printf 'tests\n' >> "$GO_RUNTIME_TEST_ROOT/actions"
  count=$(cat "$GO_RUNTIME_TEST_ROOT/test-count")
  count=$((count + 1))
  printf '%s\n' "$count" > "$GO_RUNTIME_TEST_ROOT/test-count"
  printf '%s\0' "$@" > "$GO_RUNTIME_TEST_ROOT/test-$count-args"
  printf '%s\0' "$@" >> "$GO_RUNTIME_TEST_ROOT/test-args"
  if [ "$GO_RUNTIME_FAIL_TEST_CALL" = 0 ] || [ "$GO_RUNTIME_FAIL_TEST_CALL" = "$count" ]; then
    exit "$GO_RUNTIME_TEST_STATUS"
  fi
  exit 0;;
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
		"GO_RUNTIME_TEST_ROOT="+root, "GO_RUNTIME_LIST_STATUS="+config.listStatus, "GO_RUNTIME_TEST_STATUS="+config.testStatus,
		"GO_RUNTIME_INVENTORY_STATUS="+config.inventoryStatus, "GO_RUNTIME_FAIL_TEST_CALL="+strconv.Itoa(config.failTestCall))
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

func goRuntimeTestCalls(t *testing.T, root string) [][]string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "test-count"))
	if err != nil {
		t.Fatal(err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	calls := [][]string{}
	for index := 1; index <= count; index++ {
		calls = append(calls, goRuntimeArguments(t, root, "test-"+strconv.Itoa(index)))
	}
	return calls
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
	explicit, err, output := goRuntimeFixture(t, strings.Join(packages, "\n"), "0", "0", "all")
	if err != nil || !reflect.DeepEqual(goRuntimeArguments(t, explicit, "test"), want) {
		t.Fatalf("explicit all changed the complete original scope: %v: %s", err, output)
	}
	for _, directory := range []string{root, explicit} {
		if len(goRuntimeTestCalls(t, directory)) != 1 {
			t.Fatal("complete suite was partitioned")
		}
		if _, err := os.Stat(filepath.Join(directory, "inventory-args")); !os.IsNotExist(err) {
			t.Fatal("complete suite unnecessarily selected a partial inventory", err)
		}
	}
}

func TestGoRuntimeCoreHTTPSAndInstallerCoverOriginalScopeExactlyOnce(t *testing.T) {
	packages := []string{
		goRuntimeModule + "/internal/tooling", goRuntimeModule + "/internal/backend",
		goRuntimeModule + "/internal/remote", goRuntimeModule + "/internal/remote/child",
		goRuntimeModule + "/internal/remote-extra", goRuntimeModule + "/cmd/new-command",
		goRuntimeModule + "/internal/tooling/child", goRuntimeModule + "/internal/backend/child",
	}
	backend := packages[1]
	base := []string{"test", "-p", "2", "-parallel", "2", "-race"}
	selected := map[string]int{}
	// A future test with the same name in another package must remain in core,
	// along with all examples, fuzz seeds and unrelated future backend tests.
	names := []string{goRuntimeInstallerTest, goRuntimeInstallerTest + "Suffix", "TestPrefix" + goRuntimeInstallerTest, "TestFuture", "ExampleFuture", "FuzzFuture"}
	for _, suite := range []struct {
		name      string
		calls     [][]string
		inventory bool
	}{
		{"core", [][]string{
			append(append([]string{}, base...), backend, "-skip", goRuntimeInstallerPattern, "-timeout", "12m"),
			append(append([]string{}, base...), packages[3], packages[4], packages[5], packages[6], packages[7], "-timeout", "12m"),
		}, true},
		{"https", [][]string{append(append([]string{}, base...), packages[2], "-timeout", "12m")}, false},
		{"installer", [][]string{append(append([]string{}, base...), backend, "-run", goRuntimeInstallerPattern, "-timeout", "12m")}, true},
	} {
		root, err, output := goRuntimeFixture(t, strings.Join(packages, "\n"), "0", "0", suite.name)
		if err != nil {
			t.Fatalf("%s: %v: %s", suite.name, err, output)
		}
		calls := goRuntimeTestCalls(t, root)
		if !reflect.DeepEqual(calls, suite.calls) {
			t.Fatalf("%s changed scope or flags: got %q, want %q", suite.name, calls, suite.calls)
		}
		wantActions := "packages\ntests\n"
		if suite.inventory {
			want := []string{"test", "-race", "-p", "2", "./internal/backend", "-list", goRuntimeInstallerPattern}
			if got := goRuntimeArguments(t, root, "inventory"); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s inventory lost exact race-built selection: got %q, want %q", suite.name, got, want)
			}
			wantActions = "packages\ninventory\n" + strings.Repeat("tests\n", len(calls))
		} else if _, err := os.Stat(filepath.Join(root, "inventory-args")); !os.IsNotExist(err) {
			t.Fatal("HTTPS unexpectedly enumerated an unrelated backend test", err)
		}
		actions, err := os.ReadFile(filepath.Join(root, "actions"))
		if err != nil || string(actions) != wantActions {
			t.Fatalf("%s tested before validating its selection: %v: %q", suite.name, err, actions)
		}
		for _, call := range calls {
			var run, skip *regexp.Regexp
			for index, argument := range call {
				if argument == "-run" {
					run = regexp.MustCompile(call[index+1])
				} else if argument == "-skip" {
					skip = regexp.MustCompile(call[index+1])
				}
			}
			for _, argument := range call {
				if !strings.HasPrefix(argument, goRuntimeModule+"/") {
					continue
				}
				for _, name := range names {
					if (run == nil || run.MatchString(name)) && (skip == nil || !skip.MatchString(name)) {
						selected[argument+"/"+name]++
					}
				}
			}
		}
	}
	for _, path := range packages[1:] {
		for _, name := range names {
			if selected[path+"/"+name] != 1 {
				t.Fatal("original runtime test scope omitted or duplicated", path, name, selected[path+"/"+name])
			}
		}
	}
}

func TestGoRuntimeRejectsInvalidSelectionBeforeEnumeration(t *testing.T) {
	for _, selection := range [][]string{{""}, {"unknown"}, {"core-0"}, {"core-3"}, {"--run=Nothing"}, {"core", "https"}} {
		root, err, output := goRuntimeFixture(t, "unused", "0", "0", selection...)
		if err == nil || !strings.Contains(output, "Usage:") {
			t.Fatalf("invalid selection accepted: %q: %v: %s", selection, err, output)
		}
		for _, file := range []string{"list-args", "inventory-args", "test-args"} {
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
		{"installer", goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/remote"},
		{"core-1", goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/remote"},
		{"core-2", goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/backend"},
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
		{"missing backend", tooling + "\n" + https, "0", "backend packages"},
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
			for _, scope := range []string{"all", "core", "core-1", "core-2", "https", "installer"} {
				t.Run(scope, func(t *testing.T) {
					root, err, output := goRuntimeFixture(t, test.packages, test.status, "0", scope)
					var exit *exec.ExitError
					if !errors.As(err, &exit) || !strings.Contains(output, test.diagnostic) {
						t.Fatalf("invalid enumeration was not refused: %v: %s", err, output)
					}
					if test.status != "0" && exit.ExitCode() != 23 {
						t.Fatalf("enumeration status lost: %d: %s", exit.ExitCode(), output)
					}
					for _, name := range []string{"test-args", "inventory-args"} {
						if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
							t.Fatalf("go test invoked after invalid enumeration: %s: %v", name, err)
						}
					}
				})
			}
		})
	}
}

func TestGoRuntimePreservesTestFailure(t *testing.T) {
	packages := goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/backend\n" + goRuntimeModule + "/internal/remote\n"
	for _, suite := range []string{"all", "core", "https", "installer"} {
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

func TestGoRuntimeCoreWithOnlyBackendAvoidsEmptyOtherInvocation(t *testing.T) {
	packages := goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/backend\n" + goRuntimeModule + "/internal/remote\n"
	root, err, output := goRuntimeFixture(t, packages, "0", "0", "core")
	if err != nil {
		t.Fatalf("minimal core partition failed: %v: %s", err, output)
	}
	want := [][]string{{"test", "-p", "2", "-parallel", "2", "-race", goRuntimeModule + "/internal/backend", "-skip", goRuntimeInstallerPattern, "-timeout", "12m"}}
	if got := goRuntimeTestCalls(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("minimal core tested an empty package selection: got %q, want %q", got, want)
	}
}

func TestGoRuntimeCorePreservesOtherPackageFailure(t *testing.T) {
	packages := goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/backend\n" + goRuntimeModule + "/internal/remote\n" + goRuntimeModule + "/cmd/future\n"
	config := goRuntimeDefaultConfig(packages, "0", "37")
	config.failTestCall = 2
	root, err, output := goRuntimeConfiguredFixture(t, config, "core")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 37 || len(goRuntimeTestCalls(t, root)) != 2 {
		t.Fatalf("other core package failure was lost: %v: %s", err, output)
	}
}

func TestGoRuntimePartialBackendSelectionRequiresCompleteExactInventory(t *testing.T) {
	packages := goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/backend\n" + goRuntimeModule + "/internal/remote\n" + goRuntimeModule + "/cmd/future\n"
	valid := goRuntimeInstallerTest + "\n" + goRuntimeInstallerSummary
	for _, entry := range []struct{ name, inventory, status string }{
		{"enumeration error", "", "23"},
		{"partial enumeration error", valid, "23"},
		{"empty", "", "0"},
		{"summary only", goRuntimeInstallerSummary, "0"},
		{"missing summary", goRuntimeInstallerTest + "\n", "0"},
		{"duplicate test", goRuntimeInstallerTest + "\n" + valid, "0"},
		{"duplicate summary", valid + goRuntimeInstallerSummary, "0"},
		{"test after summary", valid + goRuntimeInstallerTest + "\n", "0"},
		{"other test", "TestUnaccounted\n" + valid, "0"},
		{"benchmark", "BenchmarkUnaccounted\n" + valid, "0"},
		{"wrong package", strings.Replace(valid, "/internal/backend", "/internal/other", 1), "0"},
		{"failed summary", strings.Replace(valid, "ok\t", "FAIL\t", 1), "0"},
		{"missing duration", strings.Replace(valid, "0.123s", "", 1), "0"},
		{"truncated summary", strings.Replace(valid, "0.123s", "0.123", 1), "0"},
		{"malformed summary", strings.Replace(valid, "0.123s", "unfinished", 1), "0"},
		{"unexpected summary detail", strings.Replace(valid, "0.123s", "0.123s extra", 1), "0"},
		{"summary before test", goRuntimeInstallerSummary + goRuntimeInstallerTest + "\n", "0"},
		{"blank record", goRuntimeInstallerTest + "\n\n" + goRuntimeInstallerSummary, "0"},
		{"carriage return", goRuntimeInstallerTest + "\r\n" + goRuntimeInstallerSummary, "0"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			for _, scope := range []string{"core", "installer"} {
				t.Run(scope, func(t *testing.T) {
					config := goRuntimeDefaultConfig(packages, "0", "0")
					config.inventory, config.inventoryStatus = entry.inventory, entry.status
					root, err, output := goRuntimeConfiguredFixture(t, config, scope)
					var exit *exec.ExitError
					if !errors.As(err, &exit) || (!strings.Contains(output, "inventory") && !strings.Contains(output, "enumeration")) {
						t.Fatalf("invalid installer inventory was not refused: %v: %s", err, output)
					}
					if entry.status != "0" && exit.ExitCode() != 23 {
						t.Fatalf("inventory failure status lost: %d: %s", exit.ExitCode(), output)
					}
					if got := goRuntimeTestCalls(t, root); len(got) != 0 {
						t.Fatalf("partial tests ran after invalid inventory: %q", got)
					}
				})
			}
		})
	}
}

func TestGoRuntimeCoreShardsCoverInventoryAndPackagesExactlyOnce(t *testing.T) {
	packages := []string{goRuntimeModule + "/internal/tooling", goRuntimeModule + "/internal/backend",
		goRuntimeModule + "/internal/remote", goRuntimeModule + "/cmd/future", goRuntimeModule + "/internal/backend/child",
		goRuntimeModule + "/internal/remote/child"}
	names := []string{"TestZulu", "ExampleFuture", "FuzzFuture", "TestAlpha", goRuntimeInstallerTest + "Suffix", "TestFuture"}
	config := goRuntimeDefaultConfig(strings.Join(packages, "\n"), "0", "0")
	config.inventory = strings.Join(names, "\n") + "\nBenchmarkFuture\n" + goRuntimeInstallerTest + "\n" + goRuntimeInstallerSummary
	selected := map[string]int{}
	for _, scope := range []string{"core-1", "core-2", "https", "installer"} {
		fixture := config
		if scope == "installer" {
			fixture.inventory = goRuntimeInstallerTest + "\n" + goRuntimeInstallerSummary
		}
		root, err, output := goRuntimeConfiguredFixture(t, fixture, scope)
		if err != nil {
			t.Fatalf("%s: %v: %s", scope, err, output)
		}
		calls := goRuntimeTestCalls(t, root)
		wantCount := 1
		if scope == "core-1" {
			wantCount = 2
		}
		if len(calls) != wantCount {
			t.Fatalf("%s calls: %q", scope, calls)
		}
		if strings.HasPrefix(scope, "core-") {
			want := []string{"test", "-race", "-p", "2", "./internal/backend", "-list", "."}
			if got := goRuntimeArguments(t, root, "inventory"); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s must discover the complete race-built inventory: %q", scope, got)
			}
			pattern := "^(ExampleFuture|TestAlpha|" + goRuntimeInstallerTest + "Suffix)$"
			if scope == "core-2" {
				pattern = "^(FuzzFuture|TestFuture|TestZulu)$"
			}
			want = []string{"test", "-p", "2", "-parallel", "2", "-race", "-v", packages[1], "-run", pattern, "-timeout", "12m"}
			if !reflect.DeepEqual(calls[0], want) {
				t.Fatalf("%s changed deterministic selection, timings or budgets: %q", scope, calls[0])
			}
		}
		for _, call := range calls {
			var run *regexp.Regexp
			for index, value := range call {
				if value == "-run" {
					run = regexp.MustCompile(call[index+1])
				}
			}
			for _, pkg := range call {
				if !strings.HasPrefix(pkg, goRuntimeModule+"/") {
					continue
				}
				for _, name := range append(append([]string{}, names...), goRuntimeInstallerTest) {
					if run == nil || run.MatchString(name) {
						selected[pkg+"/"+name]++
					}
				}
			}
		}
	}
	for _, pkg := range packages[1:] {
		for _, name := range append(append([]string{}, names...), goRuntimeInstallerTest) {
			if got := selected[pkg+"/"+name]; got != 1 {
				t.Fatalf("test omitted or duplicated: %s/%s: %d", pkg, name, got)
			}
		}
	}
}

func TestGoRuntimeCoreShardsRefuseIncompleteInventory(t *testing.T) {
	packages := goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/backend\n" + goRuntimeModule + "/internal/remote\n"
	valid := "TestAlpha\nExampleFuture\nFuzzFuture\n" + goRuntimeInstallerTest + "\n" + goRuntimeInstallerSummary
	for _, test := range []struct{ name, inventory, status string }{
		{"enumeration error", valid, "23"},
		{"empty", "", "0"},
		{"missing installer", strings.Replace(valid, goRuntimeInstallerTest+"\n", "", 1), "0"},
		{"missing summary", strings.TrimSuffix(valid, goRuntimeInstallerSummary), "0"},
		{"duplicate test", "TestAlpha\n" + valid, "0"},
		{"duplicate installer", goRuntimeInstallerTest + "\n" + valid, "0"},
		{"duplicate summary", valid + goRuntimeInstallerSummary, "0"},
		{"test after summary", valid + "TestAfter\n", "0"},
		{"unsafe regex", "TestAlpha|TestOther\n" + valid, "0"},
		{"wrong package", strings.Replace(valid, "/backend", "/other", 1), "0"},
		{"invalid summary", strings.Replace(valid, "0.123s", "unfinished", 1), "0"},
		{"empty shard", "TestOnly\n" + goRuntimeInstallerTest + "\n" + goRuntimeInstallerSummary, "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, scope := range []string{"core-1", "core-2"} {
				config := goRuntimeDefaultConfig(packages, "0", "0")
				config.inventory, config.inventoryStatus = test.inventory, test.status
				root, err, output := goRuntimeConfiguredFixture(t, config, scope)
				var exit *exec.ExitError
				if !errors.As(err, &exit) || !strings.Contains(output, "inventory") || len(goRuntimeTestCalls(t, root)) != 0 {
					t.Fatalf("%s accepted invalid inventory: %v: %s", scope, err, output)
				}
				if test.status != "0" && exit.ExitCode() != 23 {
					t.Fatal("inventory failure status lost", exit.ExitCode())
				}
			}
		})
	}
}

func TestGoRuntimeCoreShardsPreserveFailures(t *testing.T) {
	packages := goRuntimeModule + "/internal/tooling\n" + goRuntimeModule + "/internal/backend\n" + goRuntimeModule + "/internal/remote\n" + goRuntimeModule + "/cmd/future\n"
	for _, scope := range []string{"core-1", "core-2"} {
		for _, call := range []int{1, 2} {
			if scope == "core-2" && call == 2 {
				continue
			}
			config := goRuntimeDefaultConfig(packages, "0", "37")
			config.inventory = "TestAlpha\nTestBeta\n" + goRuntimeInstallerTest + "\n" + goRuntimeInstallerSummary
			config.failTestCall = call
			root, err, output := goRuntimeConfiguredFixture(t, config, scope)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 37 || len(goRuntimeTestCalls(t, root)) != call {
				t.Fatalf("%s failure %d lost: %v: %s", scope, call, err, output)
			}
		}
	}
}
