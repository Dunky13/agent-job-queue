package tooling

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type resumeFixture struct {
	signingFixture
	state string
}

func newResumeFixture(t *testing.T, extra ...string) resumeFixture {
	t.Helper()
	f := newSigningFixture(t)
	var err error
	f.script, err = filepath.Abs("../../scripts/macos-resume.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"codesign", "xcrun", "ditto", "hdiutil", "spctl", "syspolicy_check", "sync", "shasum", "plutil"} {
		testTool(t, filepath.Join(f.root, "tools"), tool, resumeTestTools+signingTestTools)
	}
	f.environment = append(f.environment, signingProductionEnvironment()...)
	f.environment = append(f.environment, "GITHUB_SHA="+strings.Repeat("a", 40), "GITHUB_REPOSITORY=fixture/queue", "GITHUB_RUN_ID=1234", "GITHUB_RUN_NUMBER=21", "GITHUB_RUN_ATTEMPT=1", "SIGNING_TEST_ROOT="+f.root)
	f.environment = append(f.environment, extra...)
	fixture := resumeFixture{signingFixture: f, state: filepath.Join(f.root, "state")}
	fixture.command(t, nil, "init", f.installer, fixture.state, "0.1.0")
	return fixture
}

func (f resumeFixture) command(t *testing.T, extra []string, arguments ...string) string {
	t.Helper()
	output, err := f.run(t, extra, arguments...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return output
}

func (f resumeFixture) step(t *testing.T, operation, stage string, extra ...string) string {
	t.Helper()
	return f.command(t, extra, operation, f.state, stage, "0.1.0")
}

func (f resumeFixture) reject(t *testing.T, extra []string, want string, arguments ...string) {
	t.Helper()
	output, err := f.run(t, extra, arguments...)
	if err == nil || !strings.Contains(output, want) {
		t.Fatalf("wanted failure %q, got %v: %s", want, err, output)
	}
}

func (f resumeFixture) mutate(t *testing.T, relative string, change func(map[string]any)) {
	t.Helper()
	path := filepath.Join(f.state, relative)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	change(value)
	data, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestMacOSResumeRetainsExactArtifactsAndIDsAcrossAttempts(t *testing.T) {
	f := newResumeFixture(t)
	f.step(t, "prepare", "payload")
	original, err := os.ReadFile(filepath.Join(f.state, "archives/payload.zip"))
	if err != nil {
		t.Fatal(err)
	}
	f.step(t, "submit", "payload")
	f.reject(t, []string{"SIGNING_TEST_FAILURE=payload-timeout"}, "preserved", "finish", f.state, "payload", "0.1.0")
	f.reject(t, nil, "All three stages", "export", f.state, f.destination, "0.1.0")
	if _, err := os.Stat(f.destination); !os.IsNotExist(err) {
		t.Fatalf("pending stage created public assets: %v", err)
	}
	attempt := []string{"GITHUB_RUN_ATTEMPT=2"}
	f.step(t, "prepare", "payload", attempt...)
	f.step(t, "submit", "payload", attempt...)
	f.step(t, "finish", "payload", attempt...)
	for _, stage := range []string{"installer", "dmg"} {
		for _, operation := range []string{"prepare", "submit", "finish"} {
			f.step(t, operation, stage, attempt...)
		}
	}
	for _, stage := range []string{"payload", "installer", "dmg"} {
		for _, operation := range []string{"prepare", "submit", "finish"} {
			f.step(t, operation, stage, "GITHUB_RUN_ATTEMPT=3", "APPLE_SIGNING_IDENTITY=", "NOTARY_KEYCHAIN_PROFILE=")
		}
	}
	f.command(t, []string{"GITHUB_RUN_ATTEMPT=3", "APPLE_SIGNING_IDENTITY=", "NOTARY_KEYCHAIN_PROFILE="}, "export", f.state, f.destination, "0.1.0")
	record := f.recorded(t)
	if strings.Count(record, "xcrun|notarytool|submit|") != 3 || strings.Count(record, "codesign|--force|") != 5 {
		t.Fatalf("retry signed or submitted again:\n%s", record)
	}
	after, err := os.ReadFile(filepath.Join(f.state, "archives/payload.zip"))
	if err != nil || string(after) != string(original) {
		t.Fatal("submitted archive changed while finishing later stages")
	}
	for _, extension := range []string{"zip", "dmg"} {
		name := "Agent-Job-Queue-Installer-0.1.0-universal." + extension
		data, err := os.ReadFile(filepath.Join(f.destination, name))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		checksum, err := os.ReadFile(filepath.Join(f.destination, name+".sha256"))
		if err != nil || string(checksum) != hex.EncodeToString(hash[:])+"  "+name+"\n" {
			t.Fatalf("final checksum does not bind stapled bytes: %s, %v", checksum, err)
		}
	}
	metadata, err := os.ReadFile(filepath.Join(f.state, "metadata.json"))
	if err != nil || strings.Contains(string(metadata), f.root) || strings.Contains(string(metadata), "fixture-profile") {
		t.Fatalf("public state leaked a private path or credential: %s, %v", metadata, err)
	}
}

func TestMacOSResumeRejectsBindingAndArchiveCorruption(t *testing.T) {
	for _, test := range []struct {
		name, variable, want string
	}{
		{"team", "APPLE_TEAM_ID=AAAAAAAAAA", "metadata does not match"},
		{"commit", "GITHUB_SHA=" + strings.Repeat("b", 40), "metadata does not match"},
		{"repository", "GITHUB_REPOSITORY=other/queue", "metadata does not match"},
		{"run", "GITHUB_RUN_ID=5678", "metadata does not match"},
		{"run number", "GITHUB_RUN_NUMBER=22", "metadata does not match"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newResumeFixture(t)
			f.reject(t, []string{test.variable}, test.want, "verify-state", f.state, "0.1.0")
		})
	}
	t.Run("version", func(t *testing.T) {
		f := newResumeFixture(t)
		f.reject(t, nil, "metadata does not match", "verify-state", f.state, "0.2.0")
	})
	t.Run("initial archive", func(t *testing.T) {
		f := newResumeFixture(t)
		if err := os.WriteFile(filepath.Join(f.state, "initial.zip"), []byte("corrupt"), 0600); err != nil {
			t.Fatal(err)
		}
		f.reject(t, nil, "checksum mismatch", "verify-state", f.state, "0.1.0")
	})
	t.Run("submitted archive", func(t *testing.T) {
		f := newResumeFixture(t)
		f.step(t, "prepare", "payload")
		f.step(t, "submit", "payload")
		if err := os.WriteFile(filepath.Join(f.state, "archives/payload.zip"), []byte("corrupt"), 0600); err != nil {
			t.Fatal(err)
		}
		f.reject(t, nil, "checksum mismatch", "finish", f.state, "payload", "0.1.0")
	})
}

func TestMacOSResumeRefusesUnknownUploadAndMalformedSubmission(t *testing.T) {
	for _, failure := range []string{"payload-upload", "payload-malformed-id"} {
		t.Run(failure, func(t *testing.T) {
			f := newResumeFixture(t)
			f.step(t, "prepare", "payload")
			f.reject(t, []string{"SIGNING_TEST_FAILURE=" + failure}, "reconciliation", "submit", f.state, "payload", "0.1.0")
			f.reject(t, []string{"GITHUB_RUN_ATTEMPT=2"}, "unknown outcome", "submit", f.state, "payload", "0.1.0")
			if strings.Count(f.recorded(t), "xcrun|notarytool|submit|") != 1 {
				t.Fatal("unknown upload was resubmitted")
			}
		})
	}
	t.Run("prepared restored without identifier", func(t *testing.T) {
		f := newResumeFixture(t)
		f.step(t, "prepare", "payload")
		f.reject(t, []string{"GITHUB_RUN_ATTEMPT=2"}, "earlier attempt", "submit", f.state, "payload", "0.1.0")
		if strings.Contains(f.recorded(t), "xcrun|notarytool|submit|") {
			t.Fatal("restored prepared bytes were resubmitted")
		}
	})
	t.Run("missing recorded identifier", func(t *testing.T) {
		f := newResumeFixture(t)
		f.step(t, "prepare", "payload")
		f.step(t, "submit", "payload")
		f.mutate(t, "stages/payload.json", func(value map[string]any) { delete(value, "submission_id") })
		f.reject(t, nil, "submission ID is missing", "finish", f.state, "payload", "0.1.0")
	})
}

func TestMacOSResumeInterruptedUploadPreservesIntentAndReleasesOwnedLock(t *testing.T) {
	f := newResumeFixture(t)
	f.step(t, "prepare", "payload")
	if output, err := f.run(t, []string{"SIGNING_TEST_FAILURE=payload-interrupted"}, "submit", f.state, "payload", "0.1.0"); err == nil {
		t.Fatalf("interrupted upload unexpectedly succeeded: %s", output)
	}
	for _, relative := range []string{".lock", "work"} {
		if _, err := os.Stat(filepath.Join(f.state, relative)); !os.IsNotExist(err) {
			t.Fatalf("interrupted command retained %s: %v", relative, err)
		}
	}
	f.reject(t, []string{"GITHUB_RUN_ATTEMPT=2"}, "unknown outcome", "submit", f.state, "payload", "0.1.0")
	if strings.Count(f.recorded(t), "xcrun|notarytool|submit|") != 1 {
		t.Fatal("interrupted upload was submitted again")
	}
}

func TestMacOSResumeRetainsSanitizedAppleWarningsAndRejectionLogs(t *testing.T) {
	for _, failure := range []string{"warnings", "payload-rejected", "payload-timeout"} {
		t.Run(failure, func(t *testing.T) {
			f := newResumeFixture(t)
			f.step(t, "prepare", "payload")
			f.step(t, "submit", "payload")
			output, err := f.run(t, []string{"SIGNING_TEST_FAILURE=" + failure}, "finish", f.state, "payload", "0.1.0")
			if failure == "warnings" {
				if err != nil || !strings.Contains(output, "Apple reported issues") {
					t.Fatalf("accepted warning was not surfaced: %v, %s", err, output)
				}
			} else if err == nil {
				t.Fatalf("pending or rejected submission finished: %s", output)
			}
			data, err := os.ReadFile(filepath.Join(f.state, "receipts/payload.log.json"))
			if err != nil || !strings.Contains(string(data), "fixture diagnostic") || strings.Contains(string(data), "privateAuthentication") {
				t.Fatalf("useful Apple diagnostic was missing or unsanitized: %v, %s", err, data)
			}
			if !strings.Contains(f.recorded(t), "xcrun|notarytool|log|") {
				t.Fatal("wait result skipped the Apple log")
			}
		})
	}
}

func TestMacOSResumeRequiresExactAcceptedAppleLog(t *testing.T) {
	for _, failure := range []string{"payload-rejected", "loghash", "logid", "logstatus", "payload-log"} {
		t.Run(failure, func(t *testing.T) {
			f := newResumeFixture(t)
			f.step(t, "prepare", "payload")
			f.step(t, "submit", "payload")
			output, err := f.run(t, []string{"SIGNING_TEST_FAILURE=" + failure}, "finish", f.state, "payload", "0.1.0")
			if err == nil {
				t.Fatalf("unbound or rejected notarization was accepted: %s", output)
			}
			if strings.Contains(f.recorded(t), "xcrun|stapler|staple|") {
				t.Fatal("failed notarization reached stapling")
			}
		})
	}
}

func TestMacOSResumeRecoversInterruptedStaplingAndRejectsInvalidState(t *testing.T) {
	t.Run("stapling interruption", func(t *testing.T) {
		f := newResumeFixture(t)
		f.step(t, "prepare", "payload")
		f.step(t, "submit", "payload")
		f.reject(t, []string{"SIGNING_TEST_FAILURE=payload-staple"}, "", "finish", f.state, "payload", "0.1.0")
		// Dirty mutable workspace must never become the source of resumed output.
		if _, err := os.Stat(filepath.Join(f.state, "work")); !os.IsNotExist(err) {
			t.Fatalf("failed stage retained mutable workspace: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(f.state, "work/payload/source/bin"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.state, "work/payload/source/bin/agent-heavy"), []byte("tampered"), 0700); err != nil {
			t.Fatal(err)
		}
		f.step(t, "finish", "payload", "GITHUB_RUN_ATTEMPT=2", "APPLE_SIGNING_IDENTITY=", "NOTARY_KEYCHAIN_PROFILE=")
		if strings.Count(f.recorded(t), "xcrun|notarytool|wait|") != 1 || strings.Count(f.recorded(t), "xcrun|notarytool|submit|") != 1 {
			t.Fatal("accepted stage was resubmitted or waited again after a stapling interruption")
		}
	})
	t.Run("owned lock", func(t *testing.T) {
		f := newResumeFixture(t)
		if err := os.Mkdir(filepath.Join(f.state, ".lock"), 0700); err != nil {
			t.Fatal(err)
		}
		f.reject(t, nil, "State is locked", "verify-state", f.state, "0.1.0")
		if _, err := os.Stat(filepath.Join(f.state, ".lock")); err != nil {
			t.Fatal("another owner's lock was removed")
		}
	})
	t.Run("stage order", func(t *testing.T) {
		f := newResumeFixture(t)
		f.reject(t, nil, "previous stage", "prepare", f.state, "installer", "0.1.0")
	})
	t.Run("malformed metadata", func(t *testing.T) {
		f := newResumeFixture(t)
		if err := os.WriteFile(filepath.Join(f.state, "metadata.json"), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		f.reject(t, nil, "metadata does not match", "verify-state", f.state, "0.1.0")
	})
	t.Run("corrupt acceptance receipt", func(t *testing.T) {
		f := newResumeFixture(t)
		f.step(t, "prepare", "payload")
		f.step(t, "submit", "payload")
		f.step(t, "finish", "payload")
		f.mutate(t, "receipts/payload.json", func(value map[string]any) { value["sha256"] = strings.Repeat("0", 64) })
		f.reject(t, nil, "receipt does not match", "prepare", f.state, "installer", "0.1.0")
	})
}

// The fake archive implementation preserves contents and modes, allowing tests
// to distinguish immutable submissions from recovered and stapled workspaces.
// No fixture touches the host Keychain, signing identities or Apple services.
const resumeTestTools = `set -euo pipefail
record_event() { printf '%s' "${0##*/}" >> "$SIGNING_TEST_EVENTS"; printf '|%s' "$@" >> "$SIGNING_TEST_EVENTS"; printf '\n' >> "$SIGNING_TEST_EVENTS"; }
if [[ ${0##*/} == ditto ]]; then
  record_event "$@"
  if [[ $1 == -x ]]; then mkdir -p "$4"; tar -xf "$3" -C "$4";
  elif [[ $1 == -c ]]; then
    shift $(($# - 2)); tar -cf "$2" -C "$(dirname "$1")" "$(basename "$1")"
  else cp -R "$1" "$2"; fi
  exit
fi
if [[ ${0##*/} == xcrun && $1:$2 == notarytool:submit ]]; then
  record_event "$@"
  case "$3" in */payload.zip) number=1; stage=payload;; */installer.zip) number=2; stage=installer;; *.dmg) number=3; stage=dmg;; *) exit 92;; esac
  [[ ${SIGNING_TEST_FAILURE:-} != "$stage-upload" ]] || exit 18
  if [[ ${SIGNING_TEST_FAILURE:-} == "$stage-interrupted" ]]; then kill -TERM "$PPID"; exit 143; fi
  shasum -a 256 "$3" | awk '{print $1}' > "$SIGNING_TEST_ROOT/hash.$number"
  if [[ ${SIGNING_TEST_FAILURE:-} == "$stage-malformed-id" ]]; then printf '{}\n'; else printf '{"id":"00000000-0000-0000-0000-%012d"}\n' "$number"; fi
  exit
fi
if [[ ${0##*/} == xcrun && $1:$2 == notarytool:log ]]; then
  record_event "$@"
  case "$3" in *001) number=1; stage=payload;; *002) number=2; stage=installer;; *003) number=3; stage=dmg;; *) exit 92;; esac
  [[ ${SIGNING_TEST_FAILURE:-} != "$stage-log" ]] || exit 19
  hash=$(cat "$SIGNING_TEST_ROOT/hash.$number"); id=$3; status=Accepted
  [[ ${SIGNING_TEST_FAILURE:-} != loghash ]] || hash=0000000000000000000000000000000000000000000000000000000000000000
  [[ ${SIGNING_TEST_FAILURE:-} != logid ]] || id=00000000-0000-0000-0000-000000000009
  [[ ${SIGNING_TEST_FAILURE:-} != logstatus ]] || status=Invalid
  issues='[]'
  case "${SIGNING_TEST_FAILURE:-}" in
    warnings|payload-rejected|payload-timeout) issues='[{"severity":"warning","code":123,"message":"fixture diagnostic","path":"payload/Agent Queue.app","privateAuthentication":"must never persist"}]' ;;
  esac
  printf '{"jobId":"%s","sha256":"%s","status":"%s","issues":%s,"statusSummary":"fixture summary","privateAuthentication":"must never persist"}\n' "$id" "$hash" "$status" "$issues" > "${!#}"
  exit
fi
`
