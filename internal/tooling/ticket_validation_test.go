package tooling

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func (f *resumeFixture) injectTicketFailures(t *testing.T, operation, outcome string) {
	t.Helper()
	tools := filepath.Join(f.root, "tools")
	response := filepath.Join(tools, "ticket-response")
	testTool(t, tools, "ticket-response", ticketValidationTestTool)
	testTool(t, tools, "sleep", `printf 'sleep|%s\n' "$1" >> "$SIGNING_TEST_EVENTS"`)
	testTool(t, tools, "xcrun", `if [[ $1:$2 == stapler:$SIGNING_TEST_TICKET_OPERATION ]]; then
  "$SIGNING_TEST_TICKET_TOOL" "$@" || exit $?
fi
`+resumeTestTools+signingTestTools)
	f.environment = append(f.environment,
		"SIGNING_TEST_TICKET_TOOL="+response,
		"SIGNING_TEST_TICKET_OPERATION="+operation,
		"SIGNING_TEST_TICKET_OUTCOME="+outcome,
		"SIGNING_TEST_TICKET_COUNTER="+filepath.Join(f.root, "counter"))
}

func TestMacOSResumeRetriesStaplingWithoutResubmitting(t *testing.T) {
	for _, stage := range []string{"app", "dmg"} {
		for _, outcome := range []string{"late-recovers", "persistent", "invalid", "invalid-after-loss"} {
			t.Run(stage+"/"+outcome, func(t *testing.T) {
				f := newResumeFixture(t)
				for _, current := range []string{"app", "dmg"} {
					f.step(t, "prepare", current)
					f.step(t, "submit", current)
					if current == stage {
						break
					}
					f.step(t, "finish", current)
				}
				archive := filepath.Join(f.state, "archives", stage+".zip")
				if stage == "dmg" {
					archive = filepath.Join(f.state, "archives/dmg.dmg")
				}
				original, err := os.ReadFile(archive)
				if err != nil {
					t.Fatal(err)
				}
				before := f.recorded(t)
				f.injectTicketFailures(t, "staple", outcome)
				output, err := f.run(t, nil, "finish", f.state, stage, "0.1.0")
				phase, calls, sleeps := "accepted", 6, 5
				if outcome == "late-recovers" {
					phase = "finished"
					if err != nil {
						t.Fatalf("transient stapling failure did not recover: %v: %s", err, output)
					}
				} else {
					wantStatus := 68
					if outcome == "invalid" || outcome == "invalid-after-loss" {
						wantStatus = 65
					}
					var exitError *exec.ExitError
					if !errors.As(err, &exitError) || exitError.ExitCode() != wantStatus {
						t.Fatalf("stapling lost exit status %d: %v: %s", wantStatus, err, output)
					}
					if outcome == "persistent" && !strings.Contains(output, "exhausted 6 attempts") {
						t.Fatalf("missing retry exhaustion diagnostic: %s", output)
					}
				}
				if outcome == "invalid" {
					calls, sleeps = 1, 0
				} else if outcome == "invalid-after-loss" {
					calls, sleeps = 2, 1
				}
				record := strings.TrimPrefix(f.recorded(t), before)
				if strings.Count(record, "ticket-validation|") != calls || strings.Count(record, "sleep|") != sleeps {
					t.Fatalf("unexpected stapling attempts/backoff: %s", record)
				}
				if strings.Contains(record, "notarytool|submit|") || strings.Contains(record, "codesign|--force|") {
					t.Fatalf("ticket recovery resubmitted or signed: %s", record)
				}
				data, err := os.ReadFile(archive)
				if err != nil || !bytes.Equal(data, original) {
					t.Fatalf("ticket recovery changed submitted bytes: %v", err)
				}
				data, err = os.ReadFile(filepath.Join(f.state, "stages", stage+".json"))
				if err != nil || !strings.Contains(string(data), `"phase": "`+phase+`"`) {
					t.Fatalf("incorrect phase after stapling: %s (%v)", data, err)
				}
				if phase == "accepted" {
					f.environment = append(f.environment, "SIGNING_TEST_TICKET_OUTCOME=valid")
					before = f.recorded(t)
					f.step(t, "finish", stage, "GITHUB_RUN_ATTEMPT=2")
					record = strings.TrimPrefix(f.recorded(t), before)
					if strings.Contains(record, "notarytool|") || strings.Contains(record, "codesign|--force|") {
						t.Fatalf("accepted checkpoint was resubmitted, waited or signed again: %s", record)
					}
				}
			})
		}
	}
}

func TestMacOSResumeRetriesTicketValidationDuringDMGPreparation(t *testing.T) {
	f := newResumeFixture(t)
	for _, stage := range []string{"app"} {
		for _, operation := range []string{"prepare", "submit", "finish"} {
			f.step(t, operation, stage)
		}
	}
	before := f.recorded(t)
	f.injectTicketFailures(t, "validate", "late-recovers")
	f.step(t, "prepare", "dmg")
	record := strings.TrimPrefix(f.recorded(t), before)
	if strings.Count(record, "ticket-validation|") != 6 || strings.Count(record, "sleep|") != 5 ||
		!strings.Contains(record, "hdiutil|create|") || strings.Contains(record, "notarytool|") {
		t.Fatalf("DMG preparation did not recover without submitting: %s", record)
	}
}

func TestMacOSTicketValidationRetriesOnlyCloudKitConnectionLoss(t *testing.T) {
	for _, kind := range []string{"app", "dmg"} {
		for _, test := range []struct {
			name                  string
			status, calls, sleeps int
		}{
			{"valid", 0, 1, 0},
			{"recovers", 0, 3, 2},
			{"multiline-recovers", 0, 3, 2},
			{"late-recovers", 0, 6, 5},
			{"persistent", 68, 6, 5},
			{"invalid", 65, 1, 0},
			{"revoked", 77, 1, 0},
			{"nohost", 68, 1, 0},
			{"other-domain", 68, 1, 0},
			{"other-endpoint", 68, 1, 0},
			{"endpoint-suffix", 68, 1, 0},
			{"endpoint-child", 68, 1, 0},
			{"quoted-suffix", 68, 1, 0},
			{"quoted-prefix", 68, 1, 0},
			{"nested-suffix", 68, 1, 0},
			{"spaced-quoted-suffix", 68, 1, 0},
			{"spaced-nested-suffix", 68, 1, 0},
			{"comma-suffix", 68, 1, 0},
			{"padded-recovers", 0, 3, 2},
			{"unrelated-record", 68, 1, 0},
			{"nested-url", 68, 1, 0},
			{"quoted-url", 68, 1, 0},
			{"quoted-dictionary", 68, 1, 0},
			{"unquoted-description-url", 68, 1, 0},
			{"duplicate-url", 68, 1, 0},
			{"quoted-dictionary-recovers", 0, 3, 2},
			{"nested-recovers", 0, 3, 2},
			{"other-exit", 65, 1, 0},
			{"invalid-after-loss", 65, 2, 1},
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				f := newSigningFixture(t)
				tools := filepath.Join(f.root, "tools")
				testTool(t, tools, "xcrun", ticketValidationTestTool)
				testTool(t, tools, "sleep", `printf 'sleep|%s\n' "$1" >> "$SIGNING_TEST_EVENTS"`)
				path := f.app
				if kind == "dmg" {
					path = filepath.Join(f.root, "fixture.dmg")
					if err := os.WriteFile(path, []byte("signed fixture"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				output, err := f.run(t, []string{
					"SIGNING_TEST_TICKET_OUTCOME=" + test.name,
					"SIGNING_TEST_TICKET_COUNTER=" + filepath.Join(f.root, "counter"),
				}, "verify-"+kind, path, signingTestTeam)
				status := 0
				if err != nil {
					if exit, ok := err.(interface{ ExitCode() int }); ok {
						status = exit.ExitCode()
					} else {
						t.Fatal(err)
					}
				}
				if status != test.status {
					t.Fatalf("status=%d, want %d: %s", status, test.status, output)
				}
				record := f.recorded(t)
				if strings.Count(record, "ticket-validation|") != test.calls || strings.Count(record, "sleep|") != test.sleeps {
					t.Fatalf("unexpected validation attempts/backoff: %s", record)
				}
				for attempt, delay := 0, 5; attempt < test.sleeps; attempt, delay = attempt+1, delay*2 {
					if !strings.Contains(record, fmt.Sprintf("sleep|%d\n", delay)) {
						t.Fatalf("unexpected bounded backoff: %s", record)
					}
				}
				if strings.Contains(record, "staple|") || strings.Contains(record, "notarytool|") {
					t.Fatalf("read-only validation changed notarization state: %s", record)
				}
				entries, err := os.ReadDir(filepath.Join(f.root, "temporary"))
				if err != nil || len(entries) != 0 {
					t.Fatalf("validation left temporary output: %v %v", entries, err)
				}
			})
		}
	}
}

const ticketValidationTestTool = `set -euo pipefail
[[ $# == 3 && $1 == stapler && $2 == ${SIGNING_TEST_TICKET_OPERATION:-validate} ]] || exit 91
printf 'ticket-validation|%s\n' "$3" >> "$SIGNING_TEST_EVENTS"
count=0
if [[ -f $SIGNING_TEST_TICKET_COUNTER ]]; then count=$(cat "$SIGNING_TEST_TICKET_COUNTER"); fi
count=$((count + 1))
printf '%s\n' "$count" > "$SIGNING_TEST_TICKET_COUNTER"
url=https://api.apple-cloudkit.com/database/1/com.apple.gk.ticket-delivery/production/public/records/lookup
case "$SIGNING_TEST_TICKET_OUTCOME" in
  valid) exit 0 ;;
  recovers|multiline-recovers|nested-recovers|quoted-dictionary-recovers|padded-recovers) if (( count > 2 )); then exit 0; fi ;;
  late-recovers) if (( count > 5 )); then exit 0; fi ;;
  persistent) ;;
  invalid) printf 'The validate action failed! Error 65.\n' >&2; exit 65 ;;
  revoked) printf 'The ticket has been revoked.\n' >&2; exit 77 ;;
  nohost) printf 'CloudKit returned an unexpected response.\n' >&2; exit 68 ;;
  other-domain) printf 'Error Domain=NSCocoaErrorDomain Code=-1005 from api.apple-cloudkit.com\n' >&2; exit 68 ;;
  other-endpoint) url=https://example.invalid/ ;;
  endpoint-suffix) url=$url-other ;;
  endpoint-child) url=$url/other ;;
  quoted-suffix) url=$url'"-other"' ;;
  quoted-prefix) url='"other-"'$url ;;
  nested-suffix) url=$url'{other}' ;;
  spaced-quoted-suffix) url=$url' "-other"' ;;
  spaced-nested-suffix) url=$url' {other}' ;;
  comma-suffix) url=$url,other ;;
  padded-recovers) url="  $url  " ;;
  unrelated-record)
    printf 'error is Error Domain=NSURLErrorDomain Code=-1005 "The network connection was lost." UserInfo={NSErrorFailingURLStringKey=https://example.invalid/}\n' >&2
    printf 'error is Error Domain=NSCocoaErrorDomain Code=1 UserInfo={NSErrorFailingURLStringKey=%s}\n' "$url" >&2
    exit 68 ;;
  nested-url)
    printf 'error is Error Domain=NSURLErrorDomain Code=-1005 UserInfo={NSUnderlyingError={Error Domain=NSCocoaErrorDomain Code=1 UserInfo={NSErrorFailingURLStringKey=%s}}, NSErrorFailingURLStringKey=https://example.invalid/}\n' "$url" >&2
    exit 68 ;;
  quoted-url)
    printf 'error is Error Domain=NSURLErrorDomain Code=-1005 UserInfo={NSLocalizedDescription="diagnostic mentions NSErrorFailingURLStringKey=%s ", NSErrorFailingURLStringKey=https://example.invalid/}\n' "$url" >&2
    exit 68 ;;
  quoted-dictionary)
    printf 'error is Error Domain=NSURLErrorDomain Code=-1005 "UserInfo={NSErrorFailingURLStringKey=%s}" UserInfo={NSErrorFailingURLStringKey=https://example.invalid/}\n' "$url" >&2
    exit 68 ;;
  unquoted-description-url)
    printf 'error is Error Domain=NSURLErrorDomain Code=-1005 UserInfo={NSLocalizedDescription=Failed NSErrorFailingURLStringKey=%s, NSErrorFailingURLStringKey=https://example.invalid/}\n' "$url" >&2
    exit 68 ;;
  duplicate-url)
    printf 'error is Error Domain=NSURLErrorDomain Code=-1005 UserInfo={NSErrorFailingURLStringKey=%s, NSErrorFailingURLStringKey=https://example.invalid/}\n' "$url" >&2
    exit 68 ;;
  other-exit) ;;
  invalid-after-loss) if (( count > 1 )); then printf 'Invalid ticket.\n' >&2; exit 65; fi ;;
  *) exit 92 ;;
esac
if [[ $SIGNING_TEST_TICKET_OUTCOME == quoted-dictionary-recovers ]]; then
  printf 'error is Error Domain=NSURLErrorDomain Code=-1005 "UserInfo={NSErrorFailingURLStringKey=https://example.invalid/}" UserInfo={NSErrorFailingURLStringKey=%s}\n' "$url" >&2
elif [[ $SIGNING_TEST_TICKET_OUTCOME == nested-recovers ]]; then
  printf 'error is Error Domain=NSURLErrorDomain Code=-1005 UserInfo={NSUnderlyingError={Error Domain=NSCocoaErrorDomain Code=1 UserInfo={NSErrorFailingURLStringKey=https://example.invalid/}}, NSErrorFailingURLStringKey=%s}\n' "$url" >&2
elif [[ $SIGNING_TEST_TICKET_OUTCOME == multiline-recovers || $SIGNING_TEST_TICKET_OUTCOME == late-recovers ]]; then
  printf 'error is Error Domain=NSURLErrorDomain Code=-1005 "The network connection was lost." UserInfo={_kCFStreamErrorCodeKey=-4, NSUnderlyingError=0x1 {Error Domain=kCFErrorDomainCFNetwork Code=-1005 "(null)" UserInfo={_kCFStreamErrorCodeKey=-4, _kCFStreamErrorDomainKey=4}}, _NSURLErrorRelatedURLSessionTaskErrorKey=(\n    "LocalDataTask <fixture>.<1>"\n), NSLocalizedDescription=The network connection was lost., NSErrorFailingURLStringKey=%s, NSErrorFailingURLKey=%s, _kCFStreamErrorDomainKey=4}\n' "$url" "$url" >&2
else
  printf 'error is Error Domain=NSURLErrorDomain Code=-1005 "The network connection was lost." UserInfo={NSErrorFailingURLStringKey=%s}\n' "$url" >&2
fi
[[ $SIGNING_TEST_TICKET_OUTCOME != other-exit ]] || exit 65
exit 68
`
