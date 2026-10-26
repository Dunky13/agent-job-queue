package tooling

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func toolingShardFixture(t *testing.T, inventory string, shard []string, listStatus, testStatus string) (string, error, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "inventory"), []byte(inventory), 0600); err != nil {
		t.Fatal(err)
	}
	testTool(t, root, "go", `
[ "$1" = test ] || exit 99
case " $* " in
*" -list "*)
  pwd > "$TOOLING_SHARD_ROOT/cwd"
  printf '%s\0' "$@" > "$TOOLING_SHARD_ROOT/list-args"
  cat "$TOOLING_SHARD_ROOT/inventory"
  exit "$TOOLING_SHARD_LIST_STATUS";;
*)
  printf '%s\0' "$@" > "$TOOLING_SHARD_ROOT/test-args"
  exit "$TOOLING_SHARD_TEST_STATUS";;
esac`)
	script, err := filepath.Abs("../../scripts/test-go-tooling.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/bash", append([]string{script}, shard...)...)
	command.Dir = root
	command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TOOLING_SHARD_ROOT="+root, "TOOLING_SHARD_LIST_STATUS="+listStatus, "TOOLING_SHARD_TEST_STATUS="+testStatus)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("tooling shard did not finish: %v: %s", ctx.Err(), output)
	}
	return root, err, string(output)
}

const toolingInventorySummary = "ok\tgithub.com/Dunky13/agent-job-queue/internal/tooling\t0.001s\n"

func TestGoToolingShardsCoverCompleteInventoryExactlyOnce(t *testing.T) {
	names := []string{"TestZulu", "ExampleOutput", "FuzzInputs", "TestAlpha", "TestFuturePackageAddition"}
	inventory := strings.Join(names, "\n") + "\nBenchmarkIgnored\n" + toolingInventorySummary
	coverage := map[string]int{}
	for _, shard := range []string{"1", "2"} {
		root, err, output := toolingShardFixture(t, inventory, []string{shard}, "0", "0")
		if err != nil {
			t.Fatalf("shard %s failed: %v: %s", shard, err, output)
		}
		if got, want := goRuntimeArguments(t, root, "list"), []string{"test", "-p", "2", "./internal/tooling", "-list", "."}; !reflect.DeepEqual(got, want) {
			t.Fatalf("inventory scope: %q", got)
		}
		args := goRuntimeArguments(t, root, "test")
		if len(args) != 12 || !reflect.DeepEqual(args[:9], []string{"test", "-p", "2", "-parallel", "2", "-race", "-v", "./internal/tooling", "-run"}) || !reflect.DeepEqual(args[10:], []string{"-timeout", "12m"}) {
			t.Fatalf("race, package, worker or timeout contract changed: %q", args)
		}
		pattern, err := regexp.Compile(args[9])
		if err != nil || !strings.HasPrefix(args[9], "^(") || !strings.HasSuffix(args[9], ")$") {
			t.Fatal("shard selector is not an exact top-level inventory", args[9], err)
		}
		if pattern.MatchString("BenchmarkIgnored") || pattern.MatchString("TestAlphaUnlistedSuffix") {
			t.Fatal("selector escaped its exact inventory")
		}
		for _, name := range names {
			if pattern.MatchString(name) {
				coverage[name]++
			}
		}
		reversed := []string{"TestFuturePackageAddition", "TestAlpha", "FuzzInputs", "ExampleOutput", "TestZulu"}
		other, err, output := toolingShardFixture(t, strings.Join(reversed, "\n")+"\n"+toolingInventorySummary, []string{shard}, "0", "0")
		if err != nil || goRuntimeArguments(t, other, "test")[9] != args[9] {
			t.Fatal("inventory order changed deterministic membership", err, output)
		}
		cwd, err := os.ReadFile(filepath.Join(root, "cwd"))
		repository, pathError := filepath.Abs("../..")
		if err != nil || pathError != nil || strings.TrimSpace(string(cwd)) != repository {
			t.Fatal("enumeration did not use the repository", err, pathError)
		}
	}
	for _, name := range names {
		if coverage[name] != 1 {
			t.Fatalf("%s appears in %d shards, want exactly one", name, coverage[name])
		}
	}
}

func TestGoToolingShardsRejectIncompleteOrUnsafeInventory(t *testing.T) {
	valid := "TestAlpha\nTestBeta\n" + toolingInventorySummary
	for _, entry := range []struct{ name, inventory, status string }{
		{"list error", "", "23"},
		{"partial list error", valid, "23"},
		{"empty", "", "0"},
		{"missing summary", "TestAlpha\nTestBeta\n", "0"},
		{"duplicate", "TestAlpha\nTestAlpha\n" + toolingInventorySummary, "0"},
		{"malformed name", "TestAlpha -run=Other\nTestBeta\n" + toolingInventorySummary, "0"},
		{"blank record", "TestAlpha\n\nTestBeta\n" + toolingInventorySummary, "0"},
		{"foreign package", "TestAlpha\nTestBeta\nok example.com/foreign 0.001s\n", "0"},
		{"duplicate summary", valid + toolingInventorySummary, "0"},
		{"test after summary", valid + "TestUnaccounted\n", "0"},
		{"benchmark only", "BenchmarkOnly\n" + toolingInventorySummary, "0"},
		{"empty second shard", "TestOnly\n" + toolingInventorySummary, "0"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			root, err, output := toolingShardFixture(t, entry.inventory, []string{"1"}, entry.status, "0")
			var exited *exec.ExitError
			if !errors.As(err, &exited) || entry.status == "23" && exited.ExitCode() != 23 {
				t.Fatalf("invalid enumeration did not fail closed: %v: %s", err, output)
			}
			if _, err := os.Stat(filepath.Join(root, "test-args")); !os.IsNotExist(err) {
				t.Fatal("partial tooling tests ran after invalid inventory", err)
			}
		})
	}
}

func TestGoToolingShardsPreserveFailureAndRejectInvalidSelection(t *testing.T) {
	valid := "TestAlpha\nTestBeta\n" + toolingInventorySummary
	_, err, output := toolingShardFixture(t, valid, []string{"1"}, "0", "37")
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 37 {
		t.Fatal("test failure status was lost", err, output)
	}
	for _, args := range [][]string{nil, {"0"}, {"3"}, {"01"}, {"1", "2"}, {"-run=Other"}} {
		root, err, output := toolingShardFixture(t, valid, args, "0", "0")
		if err == nil {
			t.Fatal("invalid shard accepted", args, output)
		}
		if _, err := os.Stat(filepath.Join(root, "list-args")); !os.IsNotExist(err) {
			t.Fatal("invalid shard reached enumeration", args, err)
		}
	}
}
