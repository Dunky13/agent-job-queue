package tooling

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const appleSigningTestSHA1 = "0123456789ABCDEF0123456789ABCDEF01234567"
const appleSigningTestName = "Developer ID Application: Test (TESTTEAM01)"

func appleSigningInventories(t *testing.T, root, matching, valid string) {
	t.Helper()
	for name, content := range map[string]string{
		"identities":       "Policy: Code Signing\n  Matching identities\n" + matching + "\n  Valid identities only\n" + valid,
		"valid-identities": valid,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func appleSigningInventoryRow(hash, name string) string {
	return "  1) " + hash + " \"" + name + "\"\n"
}

func appleSigningFixture(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	appleSigningSearchListFixture(t, root)
	row := appleSigningInventoryRow(appleSigningTestSHA1, appleSigningTestName)
	appleSigningInventories(t, root, row+"     1 identities found\n", row+"     1 valid identities found\n")
	testTool(t, root, "mktemp", `printf 'mktemp\n' >> "$KEYCHAIN_TEST_ROOT/calls"
exec /usr/bin/mktemp "$@"
`)
	testTool(t, root, "base64", `printf 'base64\n' >> "$KEYCHAIN_TEST_ROOT/calls"
[ "$*" = '-D' ] || exit 99
cat >/dev/null
printf 'test certificate'
`)
	testTool(t, root, "openssl", `printf 'openssl\n' >> "$KEYCHAIN_TEST_ROOT/calls"
[ "$*" = 'rand -hex 32' ] || exit 99
printf 'random-test-password'
`)
	testTool(t, root, "security", `operation=$1
shift
printf '%s\n' "$operation" >> "$KEYCHAIN_TEST_ROOT/calls"
last=
for value in "$@"; do last=$value; done
case "$operation" in
list-keychains)
  exec "$KEYCHAIN_TEST_ROOT/search-list-tool" "$@";;
create-keychain)
  printf '%s' "$last" > "$KEYCHAIN_TEST_ROOT/keychain"
  physical_parent=$(cd -P -- "${last%/*}" && pwd -P)
  printf '%s/%s' "$physical_parent" "${last##*/}" > "$KEYCHAIN_TEST_ROOT/physical-keychain"
  touch "$last";;
unlock-keychain|set-keychain-settings|set-key-partition-list)
  [ "$last" = "$(cat "$KEYCHAIN_TEST_ROOT/keychain")" ] || exit 99;;
import)
  [ "$1" = "$(dirname "$(cat "$KEYCHAIN_TEST_ROOT/keychain")")/developer-id.p12" ] || exit 99
  [ "$2" = '-k' ] && [ "$3" = "$(cat "$KEYCHAIN_TEST_ROOT/keychain")" ] || exit 99
  [ "$last" = /usr/bin/codesign ] || exit 99;;
find-identity)
  [ "$last" = "$(cat "$KEYCHAIN_TEST_ROOT/keychain")" ] || exit 99
  case "$*" in
  '-p codesigning '*) inventory=identities;;
  '-v -p codesigning '*) inventory=valid-identities;;
  *) exit 99;;
  esac
  if [ "${KEYCHAIN_TEST_FAIL:-}" = "$inventory" ]; then
    printf '%s' 'sensitive identity query error must be suppressed' >&2
    exit 17
  fi
  cat "$KEYCHAIN_TEST_ROOT/$inventory";;
delete-keychain)
  [ "$1" = "$(cat "$KEYCHAIN_TEST_ROOT/keychain")" ] || exit 99
  if [ "${KEYCHAIN_TEST_FAIL:-}" != delete-keychain ]; then
    rm -f "$1"
    while IFS= read -r entry || [ -n "$entry" ]; do
      if [ "$entry" != "    \"$1\"" ] && [ "$entry" != "    \"$(cat "$KEYCHAIN_TEST_ROOT/physical-keychain")\"" ]; then printf '%s\n' "$entry"; fi
    done < "$KEYCHAIN_TEST_ROOT/search-state" > "$KEYCHAIN_TEST_ROOT/search-state.new"
    mv "$KEYCHAIN_TEST_ROOT/search-state.new" "$KEYCHAIN_TEST_ROOT/search-state"
  fi;;
*) exit 99;;
esac
if [ "${KEYCHAIN_TEST_FAIL:-}" = "$operation" ]; then
  printf '%s' 'sensitive tool error must be suppressed' >&2
  exit 17
fi
`)
	env := []string{
		"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMPDIR=" + root,
		"KEYCHAIN_TEST_ROOT=" + root,
		"APPLE_DEVELOPER_ID_P12_BASE64=encoded-test-certificate",
		"APPLE_DEVELOPER_ID_P12_PASSWORD=test-certificate-password",
		"APPLE_NOTARY_KEY_P8=test-api-private-key",
		"APPLE_SIGNING_IDENTITY=" + appleSigningTestName,
		"APPLE_TEAM_ID=TESTTEAM01",
		"APPLE_NOTARY_KEY_ID=TESTKEY",
		"APPLE_NOTARY_ISSUER_ID=test-issuer",
	}
	return root, env
}

func appleSigningCommand(t *testing.T, env []string, arguments ...string) *exec.Cmd {
	t.Helper()
	script, err := filepath.Abs("../../scripts/with-apple-signing.sh")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/bash", append([]string{script}, arguments...)...)
	command.Env = env
	return command
}

func appleSigningExitCode(t *testing.T, command *exec.Cmd) (int, string) {
	t.Helper()
	output, err := command.CombinedOutput()
	if err == nil {
		return 0, string(output)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatal(err)
	}
	return exitError.ExitCode(), string(output)
}

func assertAppleSigningRemoved(t *testing.T, root string) {
	t.Helper()
	keychain, err := os.ReadFile(filepath.Join(root, "keychain"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(string(keychain))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private credentials directory remains: %v", err)
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "delete-keychain\n") != 1 {
		t.Fatalf("temporary keychain was not deleted exactly once: %s", calls)
	}
	for _, operation := range strings.Fields(string(calls)) {
		switch operation {
		case "mktemp", "base64", "openssl", "create-keychain", "unlock-keychain", "set-keychain-settings", "import", "set-key-partition-list", "find-identity", "list-keychains", "delete-keychain":
		default:
			t.Fatalf("unexpected host keychain operation: %s", operation)
		}
	}
}

func assertAppleSigningProcessStopped(t *testing.T, pid int) {
	t.Helper()
	state, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			t.Fatalf("could not inspect fixture descendant: %v", err)
		}
		return
	}
	// A zombie no longer executes or accesses credentials; its new parent reaps it.
	if !strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
		t.Fatalf("credential-using descendant still running: %s", state)
	}
}

func TestAppleSigningRejectsUntrustedCIBeforeToolsAndSecrets(t *testing.T) {
	for _, test := range []struct{ event, reference string }{
		{"pull_request", "refs/heads/main"},
		{"pull_request_target", "refs/heads/main"},
		{"push", "refs/heads/feature"},
		{"workflow_dispatch", "refs/heads/main"},
		{"", ""},
	} {
		t.Run(test.event+test.reference, func(t *testing.T) {
			root, env := appleSigningFixture(t)
			// Deliberately invalid credentials must not change the refusal reason.
			env = append(env, "GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME="+test.event,
				"GITHUB_REF="+test.reference, "APPLE_NOTARY_KEY_P8=")
			code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/usr/bin/true"))
			if code == 0 || !strings.Contains(output, "trusted push to main") {
				t.Fatalf("untrusted job accepted or credentials inspected: %d %s", code, output)
			}
			if _, err := os.Stat(filepath.Join(root, "calls")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("tool invoked before trust guard: %v", err)
			}
		})
	}
}

func TestAppleSigningRequiresCompleteConfiguration(t *testing.T) {
	for _, missing := range []string{
		"APPLE_DEVELOPER_ID_P12_BASE64", "APPLE_DEVELOPER_ID_P12_PASSWORD", "APPLE_NOTARY_KEY_P8",
		"APPLE_SIGNING_IDENTITY", "APPLE_TEAM_ID", "APPLE_NOTARY_KEY_ID", "APPLE_NOTARY_ISSUER_ID",
	} {
		t.Run(missing, func(t *testing.T) {
			root, env := appleSigningFixture(t)
			env = append(env, missing+"=")
			code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/usr/bin/true"))
			if code == 0 || !strings.Contains(output, "requires "+missing) {
				t.Fatalf("missing configuration accepted: %d %s", code, output)
			}
			if _, err := os.Stat(filepath.Join(root, "calls")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("tool invoked with incomplete configuration: %v", err)
			}
		})
	}
}

func TestAppleSigningSelectsOnlyTheConfiguredValidIdentity(t *testing.T) {
	for _, selector := range []string{appleSigningTestName, appleSigningTestSHA1, strings.ToLower(appleSigningTestSHA1)} {
		t.Run(selector, func(t *testing.T) {
			root, env := appleSigningFixture(t)
			env = append(env, "APPLE_SIGNING_IDENTITY="+selector)
			code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/bin/bash", "-c", `
set -eu
[ "$APPLE_SIGNING_IDENTITY" = 0123456789ABCDEF0123456789ABCDEF01234567 ]
printf child > "$KEYCHAIN_TEST_ROOT/child"
`))
			if code != 0 || !strings.Contains(output, "classification=ready") {
				t.Fatalf("identity was not resolved before the child: %d %s", code, output)
			}
			if _, err := os.Stat(filepath.Join(root, "child")); err != nil {
				t.Fatalf("resolved identity did not reach child: %v", err)
			}
			assertAppleSigningRemoved(t, root)
		})
	}
}

func TestAppleSigningRejectsUnusableImportedIdentitiesBeforeChild(t *testing.T) {
	row := appleSigningInventoryRow(appleSigningTestSHA1, appleSigningTestName)
	other := appleSigningInventoryRow("89ABCDEF0123456789ABCDEF0123456789ABCDEF", appleSigningTestName)
	empty := "     0 identities found\n"
	emptyValid := "     0 valid identities found\n"
	for _, test := range []struct {
		name, selector, matching, valid, failure, classification string
	}{
		{"missing", appleSigningTestName, empty, emptyValid, "", "absent"},
		{"different valid identity is not a fallback", appleSigningTestSHA1, other + "     1 identities found\n", other + "     1 valid identities found\n", "", "absent"},
		{"certificate without private key", appleSigningTestSHA1, empty, emptyValid, "", "absent"},
		{"expired", appleSigningTestName, strings.TrimSuffix(row, "\n") + " (CSSMERR_TP_CERT_EXPIRED)\n     1 identities found\n", emptyValid, "", "invalid"},
		{"untrusted", appleSigningTestName, row + "     1 identities found\n", emptyValid, "", "invalid"},
		{"ambiguous names", appleSigningTestName, row + other + "     2 identities found\n", row + other + "     2 valid identities found\n", "", "ambiguous"},
		{"duplicate fingerprint", appleSigningTestSHA1, row + row + "     2 identities found\n", row + row + "     2 valid identities found\n", "", "ambiguous"},
		{"partial name", "Developer ID Application: Test", row + "     1 identities found\n", row + "     1 valid identities found\n", "", "absent"},
		{"wrong team", appleSigningTestSHA1, appleSigningInventoryRow(appleSigningTestSHA1, "Developer ID Application: Untrusted (OTHERTEAM1)") + "     1 identities found\n", appleSigningInventoryRow(appleSigningTestSHA1, "Developer ID Application: Untrusted (OTHERTEAM1)") + "     1 valid identities found\n", "", "wrong-team"},
		{"wrong certificate type", appleSigningTestSHA1, appleSigningInventoryRow(appleSigningTestSHA1, "Apple Development: Untrusted (TESTTEAM01)") + "     1 identities found\n", appleSigningInventoryRow(appleSigningTestSHA1, "Apple Development: Untrusted (TESTTEAM01)") + "     1 valid identities found\n", "", "wrong-type"},
		{"inconsistent valid fingerprint", appleSigningTestName, row + "     1 identities found\n", other + "     1 valid identities found\n", "", "inconsistent"},
		{"malformed inventory", appleSigningTestName, row + "     2 identities found\n", row + "     1 valid identities found\n", "", "malformed-inventory"},
		{"malformed valid inventory", appleSigningTestName, row + "     1 identities found\n", row + "     2 valid identities found\n", "", "malformed-inventory"},
		{"injected certificate name", appleSigningTestSHA1, appleSigningInventoryRow(appleSigningTestSHA1, "private-untrusted-name\n::error::untrusted-output") + "     1 identities found\n", emptyValid, "", "malformed-inventory"},
		{"matching query failure", appleSigningTestName, row + "     1 identities found\n", row + "     1 valid identities found\n", "identities", "query-failed"},
		{"valid query failure", appleSigningTestName, row + "     1 identities found\n", row + "     1 valid identities found\n", "valid-identities", "query-failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, env := appleSigningFixture(t)
			appleSigningInventories(t, root, test.matching, test.valid)
			env = append(env, "APPLE_SIGNING_IDENTITY="+test.selector, "KEYCHAIN_TEST_FAIL="+test.failure)
			code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/bin/bash", "-c", `printf child > "$KEYCHAIN_TEST_ROOT/child"`))
			if code == 0 || !strings.Contains(output, "classification="+test.classification) {
				t.Fatalf("unusable identity was not classified: %d %s", code, output)
			}
			if _, err := os.Stat(filepath.Join(root, "child")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("child ran with unusable identity: %v", err)
			}
			for _, forbidden := range []string{"sensitive identity query error", "private-untrusted-name", "::error::untrusted-output", "Untrusted", "test-certificate-password"} {
				if strings.Contains(output, forbidden) {
					t.Fatalf("private or untrusted diagnostic leaked: %s", output)
				}
			}
			assertAppleSigningRemoved(t, root)
		})
	}
}

func TestAppleSigningRechecksIdentityAfterChildFailure(t *testing.T) {
	for _, test := range []struct{ name, change, classification string }{
		{"no longer valid", `printf '     0 valid identities found\n' > "$KEYCHAIN_TEST_ROOT/valid-identities"`, "invalid matching=1 valid=0"},
		{"query fails", `rm "$KEYCHAIN_TEST_ROOT/valid-identities"`, "query-failed matching=unknown valid=unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, env := appleSigningFixture(t)
			code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/bin/bash", "-c", test.change+"\nexit 23\n"))
			if code != 23 || !strings.Contains(output, "phase=child-failure classification="+test.classification) {
				t.Fatalf("child failure lost its status or safe updated diagnostic: %d %s", code, output)
			}
			if strings.Contains(output, root) {
				t.Fatalf("failed diagnostic exposed a private path: %s", output)
			}
			assertAppleSigningRemoved(t, root)
		})
	}
}

func TestAppleSigningOwnsOnlyTemporaryCredentials(t *testing.T) {
	for _, test := range []struct {
		name, failure, childStatus string
		wantCode                   int
	}{
		{"success", "", "0", 0},
		{"import failure", "import", "0", 1},
		{"partial creation failure", "create-keychain", "0", 1},
		{"child failure", "", "23", 23},
		{"cleanup failure", "delete-keychain", "0", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, env := appleSigningFixture(t)
			env = append(env, "KEYCHAIN_TEST_FAIL="+test.failure, "KEYCHAIN_TEST_CHILD_STATUS="+test.childStatus,
				"GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME=push", "GITHUB_REF=refs/heads/main", "RUNNER_ENVIRONMENT=github-hosted")
			child := `
set -eu
[ "$SIGNING_MODE" = developer-id ]
[ "$APPLE_SIGNING_IDENTITY" = 0123456789ABCDEF0123456789ABCDEF01234567 ]
[ "$APPLE_SIGNING_KEYCHAIN" = "$(cat "$KEYCHAIN_TEST_ROOT/keychain")" ]
[ "$NOTARY_KEY_ID" = TESTKEY ] && [ "$NOTARY_ISSUER_ID" = test-issuer ]
[ "$(cat "$NOTARY_KEY_FILE")" = test-api-private-key ]
[ -z "${APPLE_DEVELOPER_ID_P12_BASE64+x}" ]
[ -z "${APPLE_DEVELOPER_ID_P12_PASSWORD+x}" ]
[ -z "${APPLE_NOTARY_KEY_P8+x}" ]
[ -f "$APPLE_SIGNING_KEYCHAIN" ]
[ "$(umask)" = 0077 ]
printf child > "$KEYCHAIN_TEST_ROOT/child"
exit "$KEYCHAIN_TEST_CHILD_STATUS"
`
			code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/bin/bash", "-c", child))
			if code != test.wantCode {
				t.Fatalf("exit=%d, want=%d: %s", code, test.wantCode, output)
			}
			if strings.Contains(output, "sensitive tool error") || strings.Contains(output, "test-certificate-password") {
				t.Fatalf("secret diagnostics leaked: %s", output)
			}
			_, childErr := os.Stat(filepath.Join(root, "child"))
			if (test.failure == "import" || test.failure == "create-keychain") && !errors.Is(childErr, os.ErrNotExist) {
				t.Fatal("child ran before keychain preparation completed")
			}
			if (test.failure == "" || test.failure == "delete-keychain") && childErr != nil {
				t.Fatalf("child environment verification did not finish: %v", childErr)
			}
			assertAppleSigningRemoved(t, root)
		})
	}
}

func TestAppleSigningCleansUpAfterSignals(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		for _, ignoreTerm := range []bool{false, true} {
			t.Run(signal.String()+"/ignore-term="+strconv.FormatBool(ignoreTerm), func(t *testing.T) {
				root, env := appleSigningFixture(t)
				if ignoreTerm {
					// Model process-launch/scheduling latency while the host is busy.
					// Cancellation still has the same wall-clock deadline.
					testTool(t, root, "sleep", `/bin/sleep 0.15
exec /bin/sleep "$@"
`)
				}
				env = append(env, "KEYCHAIN_TEST_IGNORE_TERM="+strconv.FormatBool(ignoreTerm))
				command := appleSigningCommand(t, env, "/bin/bash", "-c", `
if [ "$KEYCHAIN_TEST_IGNORE_TERM" = true ]; then
  trap '' TERM
else
  trap 'wait "$grandchild" 2>/dev/null; exit 0' TERM
fi
/bin/bash -c 'printf "%s" "$$" > "$KEYCHAIN_TEST_ROOT/grandchild"; exec sleep 60' &
grandchild=$!
printf child > "$KEYCHAIN_TEST_ROOT/child"
wait "$grandchild"
`)
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = command.Process.Kill() })
				deadline := time.Now().Add(5 * time.Second)
				for {
					_, childErr := os.Stat(filepath.Join(root, "child"))
					_, grandchildErr := os.Stat(filepath.Join(root, "grandchild"))
					if childErr == nil && grandchildErr == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("signing child did not start")
					}
					time.Sleep(10 * time.Millisecond)
				}
				keychain, err := os.ReadFile(filepath.Join(root, "keychain"))
				if err != nil {
					t.Fatal(err)
				}
				privateDirectory := filepath.Dir(string(keychain))
				for path, mode := range map[string]os.FileMode{
					privateDirectory: 0o700,
					filepath.Join(privateDirectory, "developer-id.p12"): 0o600,
					filepath.Join(privateDirectory, "notary-key.p8"):    0o600,
				} {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != mode {
						t.Fatalf("private credential permissions: %s, %v", path, err)
					}
				}
				pidBytes, err := os.ReadFile(filepath.Join(root, "grandchild"))
				if err != nil {
					t.Fatal(err)
				}
				grandchild, err := strconv.Atoi(string(pidBytes))
				if err != nil || grandchild <= 0 {
					t.Fatalf("invalid fixture grandchild PID: %q %v", pidBytes, err)
				}
				grandchildStopped := false
				t.Cleanup(func() {
					if !grandchildStopped {
						_ = syscall.Kill(grandchild, syscall.SIGKILL)
					}
				})
				if err := command.Process.Signal(signal); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- command.Wait() }()
				select {
				case err := <-done:
					var exitError *exec.ExitError
					if !errors.As(err, &exitError) || exitError.ExitCode() != 128+int(signal) {
						t.Fatalf("wrong signal exit status: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("signaled wrapper did not stop")
				}
				assertAppleSigningRemoved(t, root)
				assertAppleSigningProcessStopped(t, grandchild)
				grandchildStopped = true
			})
		}
	}
}

func TestAppleSigningStopsBackgroundDescendantsWhenChildExits(t *testing.T) {
	root, env := appleSigningFixture(t)
	code, output := appleSigningExitCode(t, appleSigningCommand(t, env, "/bin/bash", "-c", `
sleep 60 &
printf '%s' "$!" > "$KEYCHAIN_TEST_ROOT/grandchild"
`))
	if code != 0 {
		t.Fatalf("background fixture failed: %d %s", code, output)
	}
	pidBytes, err := os.ReadFile(filepath.Join(root, "grandchild"))
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(string(pidBytes))
	if err != nil || grandchild <= 0 {
		t.Fatalf("invalid fixture descendant PID: %q %v", pidBytes, err)
	}
	assertAppleSigningRemoved(t, root)
	assertAppleSigningProcessStopped(t, grandchild)
}
