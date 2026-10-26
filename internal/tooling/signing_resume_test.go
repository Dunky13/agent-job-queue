package tooling

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestMacOSResumeDragInstallRetainsAppPayloadAndShortcut(t *testing.T) {
	f := newResumeFixture(t)
	f.embedAppSupport(t)
	// Recreate the initial archive so the checkpoint owns the self-contained app.
	if err := os.RemoveAll(f.state); err != nil {
		t.Fatal(err)
	}
	f.command(t, nil, "init", f.installer, f.state, "0.1.0")
	for _, stage := range []string{"payload", "installer", "dmg"} {
		for _, operation := range []string{"prepare", "submit", "finish"} {
			f.step(t, operation, stage)
		}
	}
	record := f.recorded(t)
	if !strings.Contains(record, "Agent Queue.app/Contents/Resources/payload/source/bin/agent-heavy") {
		t.Fatalf("resumed release did not sign embedded helpers: %s", record)
	}
	if !strings.Contains(record, "disk-image-layout|Agent Queue.app|Applications->/Applications\n") {
		t.Fatalf("resumed release lost the drag-install layout: %s", record)
	}
}

func TestMacOSResumeRejectsDeclaredMissingPayload(t *testing.T) {
	f := newResumeFixture(t)
	app := filepath.Join(f.installer, "Contents/Resources/payload/Agent Queue.app")
	if err := os.WriteFile(filepath.Join(app, "Contents/Info.plist"), []byte(`{"AgentQueueEmbeddedPayload":"true"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.state); err != nil {
		t.Fatal(err)
	}
	f.command(t, nil, "init", f.installer, f.state, "0.1.0")
	f.reject(t, nil, "declares an embedded setup payload but it is missing", "prepare", f.state, "payload", "0.1.0")
	if _, err := os.Stat(filepath.Join(f.state, "stages/payload.json")); !os.IsNotExist(err) {
		t.Fatalf("missing payload created a prepared signing stage: %v", err)
	}
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

func newRESTResumeFixture(t *testing.T) resumeFixture {
	t.Helper()
	f := newResumeFixture(t)
	key := filepath.Join(f.root, "synthetic-notary.p8")
	if err := os.WriteFile(key, []byte("synthetic fixture key"), 0600); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(f.root, "tools", "queue-tool")
	testTool(t, filepath.Dir(tool), filepath.Base(tool), resumeRESTTestTool)
	f.environment = append(f.environment, "NOTARY_KEYCHAIN_PROFILE=", "NOTARY_KEY_FILE="+key, "NOTARY_KEY_ID=TESTKEY", "NOTARY_ISSUER_ID=00000000-0000-0000-0000-000000000001", "QUEUE_NOTARY_TOOL="+tool)
	return f
}

func TestMacOSResumeRESTAuthenticationPrecedesPreparation(t *testing.T) {
	f := newRESTResumeFixture(t)
	f.reject(t, []string{"SIGNING_TEST_FAILURE=rest-preflight"}, "operation=notary-authentication phase=failure status=33", "prepare", f.state, "payload", "0.1.0")
	events := f.recorded(t)
	if !strings.Contains(events, "queue-tool|notarize|preflight") || strings.Contains(events, "codesign|--force|") || strings.Contains(events, "ditto|-x|") {
		t.Fatalf("authentication did not stop preparation: %s", events)
	}
	if _, err := os.Stat(filepath.Join(f.state, "stages/payload.json")); !os.IsNotExist(err) {
		t.Fatalf("authentication failure created a prepared stage: %v", err)
	}
}

func TestMacOSResumeRESTRequiresPrebuiltClient(t *testing.T) {
	for _, kind := range []string{"missing", "relative", "nonexistent", "nonexecutable"} {
		t.Run(kind, func(t *testing.T) {
			f := newRESTResumeFixture(t)
			var tool string
			switch kind {
			case "relative":
				tool = "tools/queue-tool"
			case "nonexistent":
				tool = filepath.Join(f.root, "missing-client")
			case "nonexecutable":
				tool = filepath.Join(f.root, "synthetic-notary.p8")
			}
			f.reject(t, []string{"QUEUE_NOTARY_TOOL=" + tool}, "prebuilt notarization client", "prepare", f.state, "payload", "0.1.0")
			if events := f.recorded(t); strings.Contains(events, "codesign|--force|") || strings.Contains(events, "xcrun|notarytool|") || strings.Contains(events, "rest-create|") {
				t.Fatalf("missing client reached signing or notarization: %s", events)
			}
		})
	}
}

func TestMacOSResumeRESTPreservesSubmissionCustodyAcrossFailures(t *testing.T) {
	for _, test := range []struct{ failure, phase string }{{"rest-preflight", "prepared"}, {"rest-before-id", "uploading"}, {"rest-after-id", "submitted"}} {
		t.Run(test.failure, func(t *testing.T) {
			f := newRESTResumeFixture(t)
			f.step(t, "prepare", "payload")
			archive := filepath.Join(f.state, "archives/payload.zip")
			original, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/bash", f.script, "submit", f.state, "payload", "0.1.0")
			cmd.Env = append(append([]string{}, f.environment...), "SIGNING_TEST_FAILURE="+test.failure)
			output, err := cmd.Output()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) {
				t.Fatalf("failed REST submission succeeded: %s", output)
			}
			if test.phase == "submitted" {
				var result struct{ ID, Status string }
				if err := json.Unmarshal(output, &result); err != nil || result.ID != "00000000-0000-0000-0000-000000000001" || result.Status != "In Progress" {
					t.Fatalf("post-create failure lost public stdout: %s (%v)", output, err)
				}
			} else if len(output) != 0 {
				t.Fatalf("failure without a known ID emitted success stdout: %s", output)
			}
			data, err := os.ReadFile(filepath.Join(f.state, "stages/payload.json"))
			if err != nil {
				t.Fatal(err)
			}
			var record struct{ Phase, SubmissionID string }
			var value map[string]string
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			record.Phase, record.SubmissionID = value["phase"], value["submission_id"]
			if record.Phase != test.phase || (record.SubmissionID != "") != (test.phase == "submitted") {
				t.Fatalf("REST failure lost custody: %s", data)
			}
			switch test.phase {
			case "prepared":
				if strings.Contains(f.recorded(t), "rest-create|") {
					t.Fatal("failed preflight reached submission creation")
				}
			case "uploading":
				for _, attempt := range []string{"1", "2"} {
					f.reject(t, []string{"GITHUB_RUN_ATTEMPT=" + attempt}, "unknown outcome", "submit", f.state, "payload", "0.1.0")
				}
				unchanged, err := os.ReadFile(filepath.Join(f.state, "stages/payload.json"))
				if err != nil || string(unchanged) != string(data) {
					t.Fatal("unknown submission record changed on refused retries")
				}
			case "submitted":
				f.step(t, "prepare", "payload", "GITHUB_RUN_ATTEMPT=2")
				f.step(t, "submit", "payload", "GITHUB_RUN_ATTEMPT=2")
				f.step(t, "finish", "payload", "GITHUB_RUN_ATTEMPT=2")
				if events := f.recorded(t); strings.Count(events, "queue-tool|notarize|wait|") != 1 || strings.Count(events, "queue-tool|notarize|log|") != 1 {
					t.Fatalf("known submission was not queried on resume: %s", events)
				}
			}
			if after, err := os.ReadFile(archive); err != nil || string(after) != string(original) {
				t.Fatal("REST failure or retry changed immutable signed bytes")
			}
			events := f.recorded(t)
			if strings.Contains(events, "xcrun|notarytool|") || strings.Count(events, "rest-create|") > 1 || strings.Count(events, "codesign|--force|") != 3 {
				t.Fatalf("REST path repeated signing/create or used native notarization: %s", events)
			}
		})
	}
}

func TestMacOSResumeRESTRequiresExactAcceptedLog(t *testing.T) {
	for _, failure := range []string{"", "waitid", "loghash", "logid", "logstatus", "payload-log", "payload-rejected", "payload-timeout"} {
		t.Run("result="+failure, func(t *testing.T) {
			f := newRESTResumeFixture(t)
			f.step(t, "prepare", "payload")
			f.step(t, "submit", "payload")
			output, err := f.run(t, []string{"SIGNING_TEST_FAILURE=" + failure}, "finish", f.state, "payload", "0.1.0")
			if (failure == "") != (err == nil) {
				t.Fatalf("unexpected REST acceptance: %v %s", err, output)
			}
			events := f.recorded(t)
			if strings.Contains(events, "xcrun|notarytool|") || failure != "" && strings.Contains(events, "xcrun|stapler|staple|") {
				t.Fatalf("REST result bypassed policy or used native notarization: %s", events)
			}
		})
	}
}

func TestMacOSResumeRESTFinishesAllStagesWithBoundReceipts(t *testing.T) {
	f := newRESTResumeFixture(t)
	for i, stage := range []string{"payload", "installer", "dmg"} {
		output := f.step(t, "prepare", stage)
		remaining := output
		for _, operation := range []string{"configure-credentials", "notary-authentication", "restore-installer"} {
			for _, phase := range []string{"begin", "success status=0"} {
				marker := "operation=" + operation + " phase=" + phase
				index := strings.Index(remaining, marker)
				if index < 0 {
					t.Fatalf("missing ordered operation %q: %s", marker, output)
				}
				remaining = remaining[index+len(marker):]
			}
		}
		f.step(t, "submit", stage)
		f.step(t, "finish", stage)
		f.command(t, nil, "verify-state", f.state, "0.1.0")
		archive := stage + ".zip"
		if stage == "dmg" {
			archive = "dmg.dmg"
		}
		data, err := os.ReadFile(filepath.Join(f.state, "archives", archive))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		for _, name := range []string{stage + ".json", stage + ".log.json"} {
			data, err := os.ReadFile(filepath.Join(f.state, "receipts", name))
			if err != nil {
				t.Fatal(err)
			}
			var receipt struct{ JobID, SHA256, Status string }
			if err := json.Unmarshal(data, &receipt); err != nil || receipt.JobID != fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1) || receipt.SHA256 != hex.EncodeToString(hash[:]) || receipt.Status != "Accepted" || strings.Contains(string(data), "privateAuthentication") {
				t.Fatalf("unbound or unsanitized REST receipt: %s (%v)", data, err)
			}
		}
	}
	f.command(t, nil, "export", f.state, f.destination, "0.1.0")
	events := f.recorded(t)
	assertNotarySubmissionNames(t, events, "0.1.0")
	for _, event := range []string{"queue-tool|notarize|preflight", "rest-create|", "queue-tool|notarize|wait|", "queue-tool|notarize|log|", "xcrun|stapler|staple|"} {
		if strings.Count(events, event) != 3 {
			t.Fatalf("expected three %s events: %s", event, events)
		}
	}
	if strings.Contains(events, "xcrun|notarytool|") || strings.Count(events, "codesign|--force|") != 5 || !strings.Contains(events, "spctl|--assess|") {
		t.Fatalf("REST completion skipped policy or repeated signing: %s", events)
	}
}

func TestMacOSResumeRESTTimeoutCompatibility(t *testing.T) {
	for _, timeout := range []string{"300", "86400", "5m", "24h"} {
		t.Run(timeout, func(t *testing.T) {
			f := newRESTResumeFixture(t)
			env := "NOTARY_TIMEOUT=" + timeout
			f.step(t, "prepare", "payload", env)
			f.step(t, "submit", "payload", env)
			f.step(t, "finish", "payload", env)
			expected := timeout
			if timeout == "300" || timeout == "86400" {
				expected += "s"
			}
			if !strings.Contains(f.recorded(t), "|--timeout|"+expected+"\n") {
				t.Fatalf("timeout was not normalized for REST: %s", f.recorded(t))
			}
		})
	}
	for _, timeout := range []string{"86401", "1441m", "25h", strings.Repeat("9", 40)} {
		t.Run("oversized-"+timeout, func(t *testing.T) {
			f := newRESTResumeFixture(t)
			f.reject(t, []string{"NOTARY_TIMEOUT=" + timeout}, "at most 24h", "prepare", f.state, "payload", "0.1.0")
			if events := f.recorded(t); strings.Contains(events, "queue-tool|notarize|") || strings.Contains(events, "codesign|--force|") {
				t.Fatalf("invalid timeout reached signing or authentication: %s", events)
			}
		})
	}
}

func TestMacOSResumeRESTActualCLIWaitAndLogUsePrivateTemporaryPaths(t *testing.T) {
	f := newRESTResumeFixture(t)
	f.step(t, "prepare", "payload")
	f.step(t, "submit", "payload")
	tool := filepath.Join(f.root, "tools", "queue-tool-real")
	build := exec.Command("go", "build", "-p", "2", "-o", tool, "../../cmd/queue-tool")
	build.Env = signingTestEnvironment()
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build actual notarization CLI: %v\n%s", err, output)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "synthetic-notary.p8"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	// Refuse CONNECT locally, before TLS or any provider request. Both commands
	// must reach this proxy, proving real key/output parsing accepted the same
	// temporary paths used by the shell, including macOS system parent aliases.
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "appstoreconnect.apple.com:443" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
		}
		connects.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	output, err := f.run(t, []string{
		"QUEUE_NOTARY_TOOL=" + tool, "NOTARY_KEY_ID=TESTKEY001",
		"NOTARY_TIMEOUT=300",
		"HTTPS_PROXY=" + proxy.URL, "HTTP_PROXY=" + proxy.URL, "ALL_PROXY=", "NO_PROXY=",
		"https_proxy=" + proxy.URL, "http_proxy=" + proxy.URL, "all_proxy=", "no_proxy=",
	}, "finish", f.state, "payload", "0.1.0")
	if err == nil || connects.Load() != 2 || strings.Count(output, "notarization: request_failed") != 2 || !strings.Contains(output, "submission 00000000-0000-0000-0000-000000000001 is preserved") {
		t.Fatalf("real CLI did not reach both wait and log transport safely: connects=%d, %v\n%s", connects.Load(), err, output)
	}
	if strings.Contains(f.recorded(t), "xcrun|notarytool|") {
		t.Fatal("real CLI failure fell back to native notarization")
	}
	f.command(t, nil, "verify-state", f.state, "0.1.0")
}

// This trusted mock models only the CLI/state seam. Native tools and provider
// calls remain simulated; the Go client's separate tests cover real durability.
const resumeRESTTestTool = `set -euo pipefail
printf 'queue-tool' >> "$SIGNING_TEST_EVENTS"; printf '|%s' "$@" >> "$SIGNING_TEST_EVENTS"; printf '\n' >> "$SIGNING_TEST_EVENTS"
[[ $1 == notarize ]] || exit 90
operation=$2; shift 2
archive= record= hash= id= output=
while (($#)); do
  case "$1" in
    --archive) archive=$2;; --record) record=$2;; --sha256) hash=$2;;
    --id) id=$2;; --output) output=$2;; --timeout) :;; *) exit 91;;
  esac
  shift 2
done
failure=${SIGNING_TEST_FAILURE:-}
case "$operation" in
  preflight) [[ $failure != rest-preflight ]] || exit 33; exit 0;;
  submit)
    [[ $failure != rest-preflight ]] || exit 33
    [[ $(jq -r .phase "$record") == prepared && $(jq -r .archive_sha256 "$record") == "$hash" ]] || exit 92
    jq '.phase="uploading"' "$record" > "$record.new"; mv "$record.new" "$record"
    printf 'rest-create|\n' >> "$SIGNING_TEST_EVENTS"
    [[ $failure != rest-before-id ]] || exit 34
    [[ $(shasum -a 256 "$archive" | awk '{print $1}') == "$hash" ]] || exit 97
    case "$archive" in */Agent-Job-Queue-Payload-*-universal.zip) number=1;; */Agent-Job-Queue-Installer-*-universal.zip) number=2;; *.dmg) number=3;; *) exit 93;; esac
    printf -v id '00000000-0000-0000-0000-%012d' "$number"
    jq --arg id "$id" '.phase="submitted" | .submission_id=$id' "$record" > "$record.new"; mv "$record.new" "$record"
    printf '%s' "$hash" > "$SIGNING_TEST_ROOT/hash.$number"
    printf '{"id":"%s","status":"In Progress"}\n' "$id"
    [[ $failure != rest-after-id ]] || exit 35
    exit 0;;
  wait|log)
    case "$id" in *001) number=1; stage=payload;; *002) number=2; stage=installer;; *003) number=3; stage=dmg;; *) exit 94;; esac
    status=Accepted
    if [[ $operation == wait ]]; then
      [[ $failure != "$stage-rejected" ]] || status=Invalid
      [[ $failure != "$stage-timeout" ]] || status='In Progress'
      [[ $failure != waitid ]] || id=00000000-0000-0000-0000-000000000009
      printf '{"id":"%s","status":"%s"}\n' "$id" "$status"
      [[ $failure != "$stage-timeout" ]] || exit 75
    else
      [[ $failure != "$stage-log" ]] || exit 19
      [[ ! -e $output && ! -L $output ]] || exit 96
      hash=$(cat "$SIGNING_TEST_ROOT/hash.$number")
      [[ $failure != loghash ]] || hash=0000000000000000000000000000000000000000000000000000000000000000
      [[ $failure != logid ]] || id=00000000-0000-0000-0000-000000000009
      [[ $failure != logstatus ]] || status=Invalid
      printf '{"jobId":"%s","sha256":"%s","status":"%s","issues":[],"privateAuthentication":"must never persist"}\n' "$id" "$hash" "$status" > "$output"
    fi;;
  *) exit 95;;
esac
`

func TestMacOSResumePrepareReportsExactFailingOperation(t *testing.T) {
	for _, test := range []struct {
		name, failure, operation, next string
		status                         int
	}{
		{"configuration", "configuration", "configure-credentials", "restore-installer", 1},
		{"credential path", "credential-path", "configure-credentials", "restore-installer", 37},
		{"credential inside state", "credential-inside-state", "configure-credentials", "restore-installer", 1},
		{"restore", "payload-restore", "restore-installer", "sign-agent-heavy", 31},
		{"first helper", "payload-sign-heavy", "sign-agent-heavy", "verify-agent-heavy", 41},
		{"second helper", "payload-sign-remote", "sign-agent-remote", "verify-agent-remote", 42},
		{"app", "payload-sign-app", "sign-app", "verify-app", 43},
		{"verification", "payload-verify-heavy", "verify-agent-heavy", "sign-agent-remote", 44},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newResumeFixture(t)
			environment := []string{"SIGNING_TEST_FAILURE=" + test.failure}
			if test.failure == "configuration" {
				environment = append(environment, "APPLE_SIGNING_IDENTITY=")
			}
			if test.failure == "credential-path" {
				realpathTool, err := exec.LookPath("realpath")
				if err != nil {
					t.Fatal(err)
				}
				keychain := filepath.Join(f.root, "private.keychain-db")
				if err := os.WriteFile(keychain, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				testTool(t, filepath.Join(f.root, "tools"), "realpath", `if [[ $2 == "$SIGNING_TEST_CREDENTIAL_PATH" ]]; then exit 37; fi
exec "$SIGNING_TEST_REALPATH" "$@"
`)
				environment = append(environment, "APPLE_SIGNING_KEYCHAIN="+keychain, "SIGNING_TEST_CREDENTIAL_PATH="+keychain, "SIGNING_TEST_REALPATH="+realpathTool)
			}
			if test.failure == "credential-inside-state" {
				keychain := filepath.Join(f.state, "private.keychain-db")
				if err := os.WriteFile(keychain, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				environment = append(environment, "APPLE_SIGNING_KEYCHAIN="+keychain)
			}
			output, err := f.run(t, environment, "prepare", f.state, "payload", "0.1.0")
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != test.status {
				t.Fatalf("wanted original status %d, got %v: %s", test.status, err, output)
			}
			begin := "Apple signing operation: operation=" + test.operation + " phase=begin"
			failed := fmt.Sprintf("Apple signing operation: operation=%s phase=failure status=%d", test.operation, test.status)
			if !strings.Contains(output, begin) || !strings.Contains(output, failed) || strings.Index(output, begin) >= strings.Index(output, failed) {
				t.Fatalf("missing ordered safe operation evidence: %s", output)
			}
			if test.failure == "payload-sign-heavy" && !strings.Contains(output, "error: The specified item could not be found in the keychain.") {
				t.Fatalf("original tool diagnostic was lost: %s", output)
			}
			if strings.Contains(output, "operation="+test.operation+" phase=success") || strings.Contains(output, "operation="+test.next+" phase=begin") || strings.Contains(output, f.root) || strings.Contains(output, "Developer ID Application: Fixture") {
				t.Fatalf("failure continued, claimed success, or exposed private arguments: %s", output)
			}
			for _, relative := range []string{"archives/payload.zip", "stages/payload.json", "stages/payload.json.new", "work", ".lock"} {
				if _, err := os.Stat(filepath.Join(f.state, relative)); !os.IsNotExist(err) {
					t.Fatalf("failed preparation retained %s: %v", relative, err)
				}
			}
			record := f.recorded(t)
			if strings.Contains(record, "xcrun|notarytool|submit|") || strings.Contains(record, "ditto|-c|-k|--sequesterRsrc|--keepParent|"+filepath.Join(f.state, "work")) {
				t.Fatalf("failed preparation archived or submitted: %s", record)
			}
		})
	}
}

func TestMacOSResumePrepareReportsSuccessAndDoesNotRepeatOnReuse(t *testing.T) {
	f := newResumeFixture(t)
	output := f.step(t, "prepare", "payload")
	remaining := output
	for _, operation := range []string{"configure-credentials", "restore-installer", "sign-agent-heavy", "verify-agent-heavy", "sign-agent-remote", "verify-agent-remote", "sign-app", "verify-app"} {
		for _, phase := range []string{"begin", "success status=0"} {
			marker := "Apple signing operation: operation=" + operation + " phase=" + phase
			index := strings.Index(remaining, marker)
			if index < 0 {
				t.Fatalf("missing or unordered success marker %q: %s", marker, output)
			}
			remaining = remaining[index+len(marker):]
		}
	}
	if output = f.step(t, "prepare", "payload"); strings.Contains(output, "Apple signing operation:") || !strings.Contains(output, "payload already prepared") {
		t.Fatalf("reuse repeated preparation: %s", output)
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
	assertNotarySubmissionNames(t, record, "0.1.0")
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
  if [[ $1 == -x && ${SIGNING_TEST_FAILURE:-} == payload-restore ]]; then exit 31; fi
  if [[ $1 == -x ]]; then mkdir -p "$4"; tar -xf "$3" -C "$4";
  elif [[ $1 == -c ]]; then
    shift $(($# - 2)); tar -cf "$2" -C "$(dirname "$1")" "$(basename "$1")"
  else shift $(($# - 2)); cp -R "$1" "$2"; fi
  exit
fi
if [[ ${0##*/} == xcrun && $1:$2 == notarytool:submit ]]; then
  record_event "$@"
  case "$3" in */Agent-Job-Queue-Payload-*-universal.zip) number=1; stage=payload;; */Agent-Job-Queue-Installer-*-universal.zip) number=2; stage=installer;; *.dmg) number=3; stage=dmg;; *) exit 92;; esac
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

func distributionResumeIdentity() []string {
	return []string{"BUILD_RUN_ID=1234", "BUILD_RUN_NUMBER=21", "BUILD_RUN_ATTEMPT=1", "BUILD_SHA=" + strings.Repeat("a", 40), "BUILD_ARTIFACT_ID=800", "BUILD_ARTIFACT_DIGEST=sha256:" + strings.Repeat("d", 64), "BUILD_VERSION=0.1.0"}
}

func TestMacOSResumeReusesSignedBytesAndSubmissionAcrossDistributionRuns(t *testing.T) {
	identity := distributionResumeIdentity()
	first := append(append([]string{}, identity...), "GITHUB_RUN_ID=5555", "GITHUB_RUN_NUMBER=100", "GITHUB_SHA="+strings.Repeat("b", 40))
	f := newResumeFixture(t, first...)
	f.step(t, "prepare", "payload")
	original, err := os.ReadFile(filepath.Join(f.state, "archives/payload.zip"))
	if err != nil {
		t.Fatal(err)
	}
	f.step(t, "submit", "payload")
	f.reject(t, []string{"SIGNING_TEST_FAILURE=payload-timeout"}, "preserved", "finish", f.state, "payload", "0.1.0")
	second := []string{"GITHUB_RUN_ID=6666", "GITHUB_RUN_NUMBER=101", "GITHUB_SHA=" + strings.Repeat("c", 40), "GITHUB_RUN_ATTEMPT=1"}
	f.command(t, second, "verify-state", f.state, "0.1.0")
	for _, operation := range []string{"prepare", "submit", "finish"} {
		f.step(t, operation, "payload", second...)
	}
	after, err := os.ReadFile(filepath.Join(f.state, "archives/payload.zip"))
	if err != nil || string(original) != string(after) {
		t.Fatal("new distribution execution changed original signed bytes")
	}
	calls := f.recorded(t)
	if strings.Count(calls, "xcrun|notarytool|submit|") != 1 || strings.Count(calls, "codesign|--force|") != 3 {
		t.Fatalf("cross-run recovery signed or uploaded again: %s", calls)
	}
	f.mutate(t, "metadata.json", func(v map[string]any) {
		if v["run_id"] != "1234" || v["run_number"] != "21" || v["source_sha"] != strings.Repeat("a", 40) || v["artifact_id"] != "800" {
			t.Fatalf("source identity replaced by automation execution: %v", v)
		}
	})
}

func TestMacOSResumeUnknownPreparedCustodyAcrossExecutions(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			identity := distributionResumeIdentity()
			f := newResumeFixture(t, identity...)
			f.step(t, "prepare", "payload")
			if legacy {
				f.mutate(t, "stages/payload.json", func(v map[string]any) { delete(v, "prepared_run_id") })
			}
			f.reject(t, []string{"GITHUB_RUN_ID=5555", "GITHUB_RUN_NUMBER=100", "GITHUB_SHA=" + strings.Repeat("b", 40), "GITHUB_RUN_ATTEMPT=1"}, "Upload outcome is unknown", "submit", f.state, "payload", "0.1.0")
			if strings.Contains(f.recorded(t), "xcrun|notarytool|submit|") {
				t.Fatal("same numeric attempt in another execution resubmitted unknown signed bytes")
			}
		})
	}
}

func TestMacOSResumeBindsExactOriginalArtifactAndTeam(t *testing.T) {
	for _, field := range []string{"BUILD_RUN_ID", "BUILD_RUN_NUMBER", "BUILD_RUN_ATTEMPT", "BUILD_SHA", "BUILD_ARTIFACT_ID", "BUILD_ARTIFACT_DIGEST", "APPLE_TEAM_ID", "GITHUB_REPOSITORY"} {
		t.Run(field, func(t *testing.T) {
			f := newResumeFixture(t, distributionResumeIdentity()...)
			value := "999"
			switch field {
			case "BUILD_SHA":
				value = strings.Repeat("e", 40)
			case "BUILD_ARTIFACT_DIGEST":
				value = "sha256:" + strings.Repeat("e", 64)
			case "APPLE_TEAM_ID":
				value = "ZZZZZZZZZZ"
			case "GITHUB_REPOSITORY":
				value = "other/queue"
			}
			f.reject(t, []string{field + "=" + value}, "State metadata does not match", "verify-state", f.state, "0.1.0")
			if strings.Contains(f.recorded(t), "codesign|--force|") || strings.Contains(f.recorded(t), "xcrun|notarytool|") {
				t.Fatal("mismatched state reached signing/provider")
			}
		})
	}
}
