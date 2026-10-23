package tooling

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMacOSTicketValidationRetriesOnlyCloudKitConnectionLoss(t *testing.T) {
	for _, kind := range []string{"installer", "dmg"} {
		for _, test := range []struct {
			name                  string
			status, calls, sleeps int
		}{
			{"valid", 0, 1, 0},
			{"recovers", 0, 3, 2},
			{"multiline-recovers", 0, 3, 2},
			{"persistent", 68, 3, 2},
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
				path := f.installer
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
				calls := test.calls
				if kind == "installer" && status == 0 {
					calls++ // The enclosing installer also has its own ticket.
				}
				if strings.Count(record, "ticket-validation|") != calls || strings.Count(record, "sleep|") != test.sleeps {
					t.Fatalf("unexpected validation attempts/backoff: %s", record)
				}
				if test.sleeps > 0 && !strings.Contains(record, "sleep|2\n") || test.sleeps > 1 && !strings.Contains(record, "sleep|4\n") {
					t.Fatalf("unexpected bounded backoff: %s", record)
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
[[ $# == 3 && $1 == stapler && $2 == validate ]] || exit 91
printf 'ticket-validation|%s\n' "$3" >> "$SIGNING_TEST_EVENTS"
count=0
if [[ -f $SIGNING_TEST_TICKET_COUNTER ]]; then count=$(cat "$SIGNING_TEST_TICKET_COUNTER"); fi
count=$((count + 1))
printf '%s\n' "$count" > "$SIGNING_TEST_TICKET_COUNTER"
url=https://api.apple-cloudkit.com/database/1/com.apple.gk.ticket-delivery/production/public/records/lookup
case "$SIGNING_TEST_TICKET_OUTCOME" in
  valid) exit 0 ;;
  recovers|multiline-recovers|nested-recovers|quoted-dictionary-recovers|padded-recovers) if (( count > 2 )); then exit 0; fi ;;
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
elif [[ $SIGNING_TEST_TICKET_OUTCOME == multiline-recovers ]]; then
  printf 'error is Error Domain=NSURLErrorDomain Code=-1005 "The network connection was lost." UserInfo={_NSURLErrorRelatedURLSessionTaskErrorKey=(\n    "LocalDataTask <fixture>.<1>"\n), NSErrorFailingURLStringKey=%s, NSErrorFailingURLKey=%s}\n' "$url" "$url" >&2
else
  printf 'error is Error Domain=NSURLErrorDomain Code=-1005 "The network connection was lost." UserInfo={NSErrorFailingURLStringKey=%s}\n' "$url" >&2
fi
[[ $SIGNING_TEST_TICKET_OUTCOME != other-exit ]] || exit 65
exit 68
`
