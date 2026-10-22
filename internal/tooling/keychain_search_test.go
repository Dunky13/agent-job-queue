package tooling

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const appleSigningOriginalSearchList = "    \"/private/fixture/Login Keychain.keychain-db\"\n    \"/Library/Keychains/System.keychain\"\n"

func appleSigningSearchListFixture(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "search-state"), []byte(appleSigningOriginalSearchList), 0600); err != nil {
		t.Fatal(err)
	}
	testTool(t, root, "search-list-tool", `set -eu
[ "$1" = -d ] && [ "$2" = user ] || exit 99
failure=${KEYCHAIN_TEST_SEARCH_FAILURE:-}
if [ "$#" = 2 ]; then
  queries=0
  if [ -f "$KEYCHAIN_TEST_ROOT/search-queries" ]; then queries=$(cat "$KEYCHAIN_TEST_ROOT/search-queries"); fi
  queries=$((queries + 1))
  printf '%s' "$queries" > "$KEYCHAIN_TEST_ROOT/search-queries"
  if [ "$failure" = query-failure ] && [ "$queries" = 1 ]; then
    printf 'private search-list query error\n' >&2; exit 17
  fi
  cat "$KEYCHAIN_TEST_ROOT/search-state"
  exit
fi
[ "$3" = -s ] || exit 99
setters=0
if [ -f "$KEYCHAIN_TEST_ROOT/search-setters" ]; then setters=$(cat "$KEYCHAIN_TEST_ROOT/search-setters"); fi
setters=$((setters + 1))
printf '%s' "$setters" > "$KEYCHAIN_TEST_ROOT/search-setters"
printf '%s\0' "$@" > "$KEYCHAIN_TEST_ROOT/search-argv.$setters"
if { [ "$failure" = set-failure ] && [ "$setters" = 1 ]; } ||
   { [ "$failure" = restore-failure ] && [ "$setters" = 2 ]; }; then
  printf 'private search-list setter error\n' >&2; exit 17
fi
if { [ "$failure" = add-readback-mismatch ] && [ "$setters" = 1 ]; } ||
   { [ "$failure" = restore-readback-mismatch ] && [ "$setters" = 2 ]; }; then exit 0; fi
shift 3
: > "$KEYCHAIN_TEST_ROOT/search-state"
for entry in "$@"; do printf '    "%s"\n' "$entry" >> "$KEYCHAIN_TEST_ROOT/search-state"; done
if [ "$failure" = partial-set-failure ] && [ "$setters" = 1 ]; then
  printf 'private partial mutation error\n' >&2; exit 17
fi
`)
}

func appleSigningHostedSearchEnvironment(env []string) []string {
	return append(env, "GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME=push", "GITHUB_REF=refs/heads/main", "RUNNER_ENVIRONMENT=github-hosted")
}

func assertAppleSigningSearchArguments(t *testing.T, root string, call int, want []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "search-argv."+strconv.Itoa(call)))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("search-list argument boundaries or order changed: got %q, want %q", got, want)
	}
}

func assertAppleSigningSearchState(t *testing.T, root, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "search-state"))
	if err != nil || string(data) != want {
		t.Fatalf("unexpected search-list state: %q %v", data, err)
	}
}

const appleSigningVisibleChild = `set -eu
grep -Fxq "    \"$APPLE_SIGNING_KEYCHAIN\"" "$KEYCHAIN_TEST_ROOT/search-state" || exit 98
printf child > "$KEYCHAIN_TEST_ROOT/child"
exit "${KEYCHAIN_TEST_CHILD_STATUS:-0}"
`

func TestAppleSigningHostedSearchRegistersAndRestoresExactOrder(t *testing.T) {
	for _, original := range []string{appleSigningOriginalSearchList, ""} {
		for _, status := range []string{"0", "23"} {
			t.Run("entries="+strconv.Itoa(strings.Count(original, "\n"))+"/child="+status, func(t *testing.T) {
				root, env := appleSigningFixture(t)
				if err := os.WriteFile(filepath.Join(root, "search-state"), []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				env = append(appleSigningHostedSearchEnvironment(env), "KEYCHAIN_TEST_CHILD_STATUS="+status)
				code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/bin/bash", "-c", appleSigningVisibleChild))
				wantCode, _ := strconv.Atoi(status)
				if code != wantCode || !strings.Contains(output, "phase=register classification=ready") || !strings.Contains(output, "phase=restore classification=ready") {
					t.Fatalf("registration or restoration failed: %d %s", code, output)
				}
				keychain, err := os.ReadFile(filepath.Join(root, "keychain"))
				if err != nil {
					t.Fatal(err)
				}
				originalArgs := []string{"-d", "user", "-s"}
				if original != "" {
					originalArgs = append(originalArgs, "/private/fixture/Login Keychain.keychain-db", "/Library/Keychains/System.keychain")
				}
				assertAppleSigningSearchArguments(t, root, 1, append(append([]string{}, originalArgs...), string(keychain)))
				assertAppleSigningSearchArguments(t, root, 2, originalArgs)
				assertAppleSigningSearchState(t, root, original)
				if strings.Contains(output, root) || strings.Contains(output, "Login Keychain") {
					t.Fatalf("search-list paths leaked: %s", output)
				}
				assertAppleSigningRemoved(t, root)
			})
		}
	}
}

func TestAppleSigningHostedSearchRejectsMalformedOriginalWithoutSetting(t *testing.T) {
	for name, original := range map[string]string{
		"unquoted":   "/private/fixture/login.keychain-db\n",
		"relative":   "    \"relative.keychain-db\"\n",
		"escaped":    "    \"/private/fixture/escaped\\name\"\n",
		"extra text": "    \"/private/fixture/login\" injected\n",
		"control":    "    \"/private/fixture/secret\tname\"\n",
		"nul":        "    \"/private/fixture/secret\x00name\"\n",
		"duplicate":  "    \"/private/fixture/login\"\n    \"/private/fixture/login\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			root, env := appleSigningFixture(t)
			if err := os.WriteFile(filepath.Join(root, "search-state"), []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			code, output := appleSigningExitCode(t, appleSigningCommand(t, appleSigningHostedSearchEnvironment(env), "/bin/bash", "-c", `printf child > "$KEYCHAIN_TEST_ROOT/child"`))
			if code == 0 || !strings.Contains(output, "classification=malformed") || strings.Contains(output, root) || strings.Contains(output, "secret") {
				t.Fatalf("malformed search list accepted or leaked: %d %s", code, output)
			}
			for _, file := range []string{"search-setters", "child"} {
				if _, err := os.Stat(filepath.Join(root, file)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("malformed search list reached %s: %v", file, err)
				}
			}
			assertAppleSigningRemoved(t, root)
		})
	}
}

func TestAppleSigningHostedSearchFailuresRemainClosedAndCleanUp(t *testing.T) {
	for _, test := range []struct {
		failure, classification string
		child                   bool
		setters                 int
	}{
		{"query-failure", "query-failed", false, 0},
		{"set-failure", "set-failed", false, 1},
		{"partial-set-failure", "set-failed", false, 2},
		{"add-readback-mismatch", "readback-mismatch", false, 1},
		{"restore-failure", "set-failed", true, 2},
		{"restore-readback-mismatch", "readback-mismatch", true, 2},
	} {
		t.Run(test.failure, func(t *testing.T) {
			root, env := appleSigningFixture(t)
			env = append(appleSigningHostedSearchEnvironment(env), "KEYCHAIN_TEST_SEARCH_FAILURE="+test.failure)
			code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/bin/bash", "-c", appleSigningVisibleChild))
			if code == 0 || !strings.Contains(output, "classification="+test.classification) || strings.Contains(output, "private ") || strings.Contains(output, root) {
				t.Fatalf("search-list failure was bypassed or leaked: %d %s", code, output)
			}
			_, childErr := os.Stat(filepath.Join(root, "child"))
			if test.child != (childErr == nil) {
				t.Fatalf("unexpected child execution: %v", childErr)
			}
			data, err := os.ReadFile(filepath.Join(root, "search-setters"))
			if test.setters == 0 && !errors.Is(err, os.ErrNotExist) || test.setters != 0 && (err != nil || string(data) != strconv.Itoa(test.setters)) {
				t.Fatalf("unexpected setter count: %q %v", data, err)
			}
			assertAppleSigningSearchState(t, root, appleSigningOriginalSearchList)
			assertAppleSigningRemoved(t, root)
		})
	}
}

func TestAppleSigningHostedSearchPreservesConcurrentAdditions(t *testing.T) {
	root, env := appleSigningFixture(t)
	code, output := appleSigningExitCode(t, appleSigningCommand(t, appleSigningHostedSearchEnvironment(env), "/bin/bash", "-c", `
printf '    "/private/fixture/concurrent.keychain-db"\n' >> "$KEYCHAIN_TEST_ROOT/search-state"
printf child > "$KEYCHAIN_TEST_ROOT/child"
`))
	if code == 0 || !strings.Contains(output, "phase=restore classification=conflict") {
		t.Fatalf("concurrent changes were overwritten: %d %s", code, output)
	}
	assertAppleSigningSearchState(t, root, appleSigningOriginalSearchList+"    \"/private/fixture/concurrent.keychain-db\"\n")
	assertAppleSigningRemoved(t, root)
}

func TestAppleSigningHostedSearchRefusesOtherRunnerEnvironmentsBeforeTools(t *testing.T) {
	for _, runner := range []string{"", "self-hosted", "github-hosted-other"} {
		t.Run(runner, func(t *testing.T) {
			root, env := appleSigningFixture(t)
			env = append(appleSigningHostedSearchEnvironment(env), "RUNNER_ENVIRONMENT="+runner, "APPLE_NOTARY_KEY_P8=")
			code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/usr/bin/true"))
			if code == 0 || !strings.Contains(output, "GitHub-hosted runner") {
				t.Fatalf("unsafe runner accepted or credentials inspected: %d %s", code, output)
			}
			if _, err := os.Stat(filepath.Join(root, "calls")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("tools ran before runner guard: %v", err)
			}
		})
	}
}

func TestAppleSigningLocalDoesNotReadOrWriteSearchList(t *testing.T) {
	root, env := appleSigningFixture(t)
	code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/usr/bin/true"))
	if code != 0 {
		t.Fatalf("local wrapper failed: %d %s", code, output)
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil || strings.Contains(string(calls), "list-keychains") {
		t.Fatalf("local wrapper touched search list: %s %v", calls, err)
	}
	assertAppleSigningRemoved(t, root)
}

func TestAppleSigningSearchHelperGuardsSettingsBeforeTools(t *testing.T) {
	module, err := filepath.Abs("../../scripts/macos-keychain-search.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, context := range []struct{ actions, runner string }{{"", "github-hosted"}, {"true", ""}, {"true", "self-hosted"}} {
		t.Run(context.actions+"/"+context.runner, func(t *testing.T) {
			root, env := appleSigningFixture(t)
			command := exec.Command("/bin/bash", "-c", `set -euo pipefail
source "$1"
macos_keychain_search_register "$2"
`, "fixture", module, filepath.Join(root, "missing-owned-keychain"))
			command.Env = append(env, "GITHUB_ACTIONS="+context.actions, "RUNNER_ENVIRONMENT="+context.runner)
			code, output := appleSigningExitCode(t, command)
			if code == 0 || !strings.Contains(output, "classification=forbidden-runner") || strings.Contains(output, root) {
				t.Fatalf("helper did not enforce runner boundary first: %d %s", code, output)
			}
			if _, err := os.Stat(filepath.Join(root, "calls")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("helper touched tools before runner guard: %v", err)
			}
		})
	}
}

func TestAppleSigningSearchHelperCanRestoreThenRegisterAnEmptyList(t *testing.T) {
	root, env := appleSigningFixture(t)
	module, err := filepath.Abs("../../scripts/macos-keychain-search.sh")
	if err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(root, "owned.keychain-db")
	if err := os.WriteFile(owned, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/bash", "-c", `set -euo pipefail
source "$1"
macos_keychain_search_register "$2"
macos_keychain_search_restore
: > "$KEYCHAIN_TEST_ROOT/search-state"
macos_keychain_search_register "$2"
macos_keychain_search_restore
`, "fixture", module, owned)
	command.Env = appleSigningHostedSearchEnvironment(env)
	if code, output := appleSigningExitCode(t, command); code != 0 {
		t.Fatalf("sequential helper registration failed: %d %s", code, output)
	}
	assertAppleSigningSearchArguments(t, root, 3, []string{"-d", "user", "-s", owned})
	assertAppleSigningSearchArguments(t, root, 4, []string{"-d", "user", "-s"})
	assertAppleSigningSearchState(t, root, "")
}

func TestAppleSigningHostedSearchRestoresAfterCancelledChildStops(t *testing.T) {
	root, env := appleSigningFixture(t)
	command := appleSigningCommand(t, appleSigningHostedSearchEnvironment(env), "/bin/bash", "-c", `
trap 'printf "child-stopped\n" >> "$KEYCHAIN_TEST_ROOT/calls"; exit 0' TERM
printf child > "$KEYCHAIN_TEST_ROOT/child"
while :; do sleep 1; done
`)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "child")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hosted mock child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
		if command.ProcessState.ExitCode() != 143 {
			t.Fatalf("wrong cancellation status: %s", command.ProcessState)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled hosted mock wrapper did not finish")
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	stopped := strings.Index(string(calls), "child-stopped\n")
	restored := strings.LastIndex(string(calls), "list-keychains\n")
	deleted := strings.Index(string(calls), "delete-keychain\n")
	if stopped < 0 || restored < stopped || deleted < restored {
		t.Fatalf("restoration did not follow child stop and precede deletion: %s", calls)
	}
	assertAppleSigningSearchState(t, root, appleSigningOriginalSearchList)
	// This marker belongs to the simulated child, not an extra security operation.
	if err := os.WriteFile(filepath.Join(root, "calls"), []byte(strings.ReplaceAll(string(calls), "child-stopped\n", "")), 0600); err != nil {
		t.Fatal(err)
	}
	assertAppleSigningRemoved(t, root)
}
