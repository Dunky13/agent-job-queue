package tooling

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type distributionRecoveryFixture struct {
	checkpointFixture
	execution, source, recovery map[string]any
	documents                   map[string]any
	artifacts, originals        []map[string]any
	jobs, gates, app            []map[string]any
	otherRuns                   []map[string]any
	extraEntries                []checkpointEntry
	archive                     []byte
}

func newDistributionRecoveryFixture(t *testing.T) *distributionRecoveryFixture {
	t.Helper()
	f := newCheckpointFixture(t)
	f.script, _ = filepath.Abs("../../scripts/distribution-recover.sh")
	// The observer deliberately needs neither a Distribution execution context
	// nor an Apple client, credentials, or a source checkout.
	f.env = append(f.env, "GITHUB_EVENT_NAME=schedule", "GITHUB_RUN_ID=", "GITHUB_RUN_NUMBER=", "GITHUB_RUN_ATTEMPT=", "GITHUB_SHA=")
	testTool(t, filepath.Join(f.root, "tools"), "gh", `printf '%s\n' "$*" >> "$CHECKPOINT_TEST_ROOT/calls"
[ "$1" = api ] || exit 99
shift
if [ "$1" = --method ]; then
  [ "$2" = POST ] || exit 98
  case "$3" in repos/owner/project/actions/runs/*/rerun-failed-jobs) exit "${RECOVERY_POST_RESULT:-0}";; *) exit 97;; esac
fi
case "$1" in
 repos/owner/project/actions/workflows/distribution.yml/runs\?branch=main\&per_page=100\&page=*)
   page=${1##*=}
   if [ -f "$CHECKPOINT_TEST_ROOT/fresh-$page.json" ] && [ -f "$CHECKPOINT_TEST_ROOT/observed" ]; then
     cat "$CHECKPOINT_TEST_ROOT/fresh-$page.json"
   else
     cat "$CHECKPOINT_TEST_ROOT/runs-$page.json"
   fi
   [ "$page" != 1 ] || touch "$CHECKPOINT_TEST_ROOT/observed";;
 repos/owner/project/actions/artifacts/*/zip)
   id=${1%/zip};id=${id##*/};cat "$CHECKPOINT_TEST_ROOT/artifact-$id.zip";;
 repos/owner/project/actions/runs/*)
   tail=${1#*/runs/}; id=${tail%%/*}
   case "$tail" in
     */artifacts\?*) cat "$CHECKPOINT_TEST_ROOT/artifacts-$id.json";;
     */attempts/*/jobs\?*) cat "$CHECKPOINT_TEST_ROOT/app-$id.json";;
     */jobs\?filter=latest\&*) cat "$CHECKPOINT_TEST_ROOT/jobs-$id.json";;
     *\?*) exit 96;;
     *) cat "$CHECKPOINT_TEST_ROOT/run-$id.json";;
   esac;;
 *) exit 95;;
esac
`)
	testTool(t, filepath.Join(f.root, "tools"), "xcrun", `printf 'unexpected Apple operation\n' >> "$CHECKPOINT_TEST_ROOT/apple-calls"; exit 99`)
	return distributionRecoveryCandidate(f, 999, 123)
}

func distributionRecoveryCandidate(f checkpointFixture, id, build int) *distributionRecoveryFixture {
	sha := strings.Repeat("b", 40)
	provenance := func(run int, commit string) map[string]any {
		return map[string]any{"id": run, "head_branch": "main", "head_sha": commit}
	}
	r := &distributionRecoveryFixture{checkpointFixture: f}
	r.execution = map[string]any{
		"id": id, "run_attempt": 2, "display_title": fmt.Sprintf("Distribute build %d", build),
		"head_sha": sha, "path": ".github/workflows/distribution.yml", "event": "workflow_dispatch", "head_branch": "main",
		"status": "completed", "conclusion": "failure", "created_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"updated_at": time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339),
		"repository": map[string]any{"full_name": "owner/project"}, "head_repository": map[string]any{"full_name": "owner/project"},
	}
	r.source = map[string]any{
		"id": build, "run_number": 45, "run_attempt": 3, "head_sha": checkpointSHA,
		"path": ".github/workflows/ci.yml", "event": "push", "head_branch": "main", "status": "completed", "conclusion": "failure",
		"repository": map[string]any{"full_name": "owner/project"}, "head_repository": map[string]any{"full_name": "owner/project"},
	}
	r.recovery = map[string]any{
		"schema": 1, "repository": "owner/project", "source_sha": checkpointSHA, "build_run_id": fmt.Sprint(build),
		"build_run_number": "45", "build_attempt": "1", "build_artifact_id": "800", "build_artifact_digest": "sha256:" + strings.Repeat("c", 64),
		"version": "0.1.0", "team": "ABCDEFGHIJ", "execution_id": fmt.Sprint(id), "execution_attempt": "2", "automation_sha": sha,
		"checkpoint_id": "900", "checkpoint_digest": "sha256:" + strings.Repeat("d", 64),
		"stage": "app", "phase": "submitted", "reason": "pending", "submission_id": "00000000-0000-0000-0000-000000000001", "archive_sha256": strings.Repeat("e", 64),
	}
	r.documents = map[string]any{
		"recovery.json": r.recovery,
		"metadata.json": map[string]any{"schema": 2, "repository": "owner/project", "source_sha": checkpointSHA, "run_id": fmt.Sprint(build), "run_number": "45",
			"build_attempt": "1", "artifact_id": "800", "artifact_digest": r.recovery["build_artifact_digest"], "version": "0.1.0", "team": "ABCDEFGHIJ", "initial_sha256": strings.Repeat("f", 64)},
		"stage-app.json":  map[string]any{"stage": "app", "phase": "submitted", "prepared_run_id": fmt.Sprint(id), "prepared_attempt": "2", "submission_id": r.recovery["submission_id"], "archive_sha256": r.recovery["archive_sha256"], "completed_sha256": ""},
		"status-app.json": map[string]any{"id": r.recovery["submission_id"], "status": "In Progress", "error": "timeout", "execution_id": fmt.Sprint(id), "execution_attempt": "2"},
	}
	r.artifacts = []map[string]any{
		{"id": 900, "name": fmt.Sprintf("Apple-notarization-b%d-r%d-a2-s03", build, id), "expired": false, "digest": r.recovery["checkpoint_digest"], "workflow_run": provenance(id, sha)},
		{"id": 901, "name": fmt.Sprintf("Apple-receipts-b%d-r%d-a2-s03", build, id), "expired": false, "workflow_run": provenance(id, sha)},
	}
	r.originals = []map[string]any{{"id": 800, "name": "Agent-Queue-App-1", "expired": false, "digest": r.recovery["build_artifact_digest"], "workflow_run": provenance(build, checkpointSHA)}}
	job := func(jobID, run int, name, conclusion, commit string, attempt int) map[string]any {
		return map[string]any{"id": jobID, "run_id": run, "name": name, "status": "completed", "conclusion": conclusion, "head_sha": commit, "run_attempt": attempt}
	}
	r.jobs = []map[string]any{
		job(10, id, "Verify original build provenance", "success", sha, 1), job(11, id, "Sign and notarize main installer", "failure", sha, 2),
		job(12, id, "Notarized distribution (macos-15)", "skipped", sha, 2), job(13, id, "Notarized distribution (macos-15-intel)", "skipped", sha, 2),
		job(14, id, "Publish main installer", "skipped", sha, 2), job(15, id, "Verify Homebrew delivery", "skipped", sha, 2),
	}
	r.gates = []map[string]any{job(20, build, "ci-required", "success", checkpointSHA, 3)}
	r.app = []map[string]any{job(21, build, "Universal app", "success", checkpointSHA, 1)}
	return r
}

func (f *distributionRecoveryFixture) document(name string) map[string]any {
	return f.documents[name].(map[string]any)
}

func (f *distributionRecoveryFixture) write(t *testing.T) {
	t.Helper()
	id, build := f.execution["id"], f.source["id"]
	f.writeJSON(t, fmt.Sprintf("run-%v.json", id), f.execution)
	f.writeJSON(t, fmt.Sprintf("run-%v.json", build), f.source)
	for filename, inventory := range map[string][]map[string]any{
		fmt.Sprintf("jobs-%v.json", id): f.jobs, fmt.Sprintf("jobs-%v.json", build): f.gates, fmt.Sprintf("app-%v.json", build): f.app,
	} {
		f.writeJSON(t, filename, map[string]any{"total_count": len(inventory), "jobs": inventory})
	}
	entries := []checkpointEntry{}
	names := make([]string, 0, len(f.documents))
	for name := range f.documents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := json.Marshal(f.documents[name])
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, checkpointEntry{name, data, 0o600})
	}
	entries = append(entries, f.extraEntries...)
	archive := f.archive
	if archive == nil {
		archive = checkpointZIP(t, entries...)
	}
	if err := os.WriteFile(filepath.Join(f.root, "artifact-901.zip"), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	if f.artifacts[1]["digest"] == nil {
		f.artifacts[1]["digest"] = fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
	}
	f.writeJSON(t, fmt.Sprintf("artifacts-%v.json", id), map[string]any{"total_count": len(f.artifacts), "artifacts": f.artifacts})
	f.writeJSON(t, fmt.Sprintf("artifacts-%v.json", build), map[string]any{"total_count": len(f.originals), "artifacts": f.originals})
	f.writeJSON(t, "runs-1.json", map[string]any{"total_count": 1 + len(f.otherRuns), "workflow_runs": append([]map[string]any{f.execution}, f.otherRuns...)})
}

func (f *distributionRecoveryFixture) observe(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	f.write(t)
	return f.run(t, extra)
}

func (f *distributionRecoveryFixture) assertMutations(t *testing.T, want int) {
	t.Helper()
	body, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	count := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, "--method") {
			if !strings.HasPrefix(line, "api --method POST repos/owner/project/actions/runs/") || !strings.HasSuffix(line, "/rerun-failed-jobs") {
				t.Fatalf("unexpected provider mutation: %s", line)
			}
			count++
		}
		for _, forbidden := range []string{"dispatch", "rerun/", "--method PUT", "--method DELETE"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("unexpected replay/dispatch: %s", line)
			}
		}
	}
	if count != want {
		t.Fatalf("got %d mutations, want %d: %s", count, want, body)
	}
	if _, err := os.Stat(filepath.Join(f.root, "apple-calls")); !os.IsNotExist(err) {
		t.Fatal("observer attempted to invoke Apple tooling")
	}
}

func TestDistributionRecoveryPendingUsesExactFailedRun(t *testing.T) {
	t.Parallel()
	for _, event := range []string{"workflow_run", "workflow_dispatch", "schedule"} {
		t.Run(event, func(t *testing.T) {
			f := newDistributionRecoveryFixture(t)
			output, err := f.observe(t, "GITHUB_EVENT_NAME="+event)
			if err != nil || !strings.Contains(output, "build=123 execution=999 next_attempt=3") {
				t.Fatalf("pending continuation: %v: %s", err, output)
			}
			f.assertMutations(t, 1)
			calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
			if !strings.Contains(string(calls), "api --method POST repos/owner/project/actions/runs/999/rerun-failed-jobs\n") ||
				strings.Contains(string(calls), "/artifacts/900/zip") || strings.Contains(string(calls), "/artifacts/800/zip") {
				t.Fatalf("observer must inspect small receipts and rerun the original execution: %s", calls)
			}
		})
	}
}

func TestDistributionRecoveryRequiresTrustedObserver(t *testing.T) {
	t.Parallel()
	for _, environment := range []string{"GITHUB_ACTIONS=false", "GITHUB_REF=refs/heads/feature", "GITHUB_REPOSITORY=../owner/project", "GITHUB_REPOSITORY=owner/..", "GITHUB_REPOSITORY=./project", "GITHUB_EVENT_NAME=push", "GITHUB_EVENT_NAME=pull_request_target"} {
		t.Run(environment, func(t *testing.T) {
			f := newDistributionRecoveryFixture(t)
			if output, err := f.observe(t, environment); err == nil {
				t.Fatalf("untrusted observer accepted: %s", output)
			}
			f.assertMutations(t, 0)
		})
	}
}

func TestDistributionRecoveryRefusesUnboundOrTerminalEvidence(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{
		"schema", "extra key", "string schema", "numeric context", "repository", "source hash", "execution", "attempt binding", "automation hash", "build title", "uuid", "archive hash", "team", "version",
		"bad digest", "missing digest", "expired receipts", "receipt run", "receipt sha", "receipt branch", "missing receipts", "old attempt receipts", "malformed slot", "duplicate slot",
		"missing checkpoint", "expired checkpoint", "checkpoint digest", "checkpoint run", "checkpoint sha", "checkpoint slot", "newer checkpoint", "newer receipt missing", "newer receipt expired",
		"missing metadata", "metadata binding", "missing stage", "stage binding", "missing status", "status id", "rejected", "invalid", "none", "pending phase", "ticket phase", "rejected log", "log mismatch", "log filename", "nonaccepted log", "rejected receipt", "other stage rejection",
		"source fork", "source branch", "source event", "source path", "source build", "source attempt", "source sha", "source number", "source active", "source gate", "missing gate", "gate sha", "gate attempt", "app failure",
		"missing original", "expired original", "original digest", "original attempt", "original sha", "original run", "cancelled", "attempt limit", "old run", "future run", "malformed date",
		"failed validation", "other failed job", "cancelled job", "duplicate job", "wrong job sha", "wrong job attempt", "wrong job run", "old signing failure",
	} {
		t.Run(variant, func(t *testing.T) {
			f := newDistributionRecoveryFixture(t)
			stage := f.document("stage-app.json")
			switch variant {
			case "schema":
				f.recovery["schema"] = 2
			case "extra key":
				f.recovery["unexpected"] = "value"
			case "string schema":
				f.recovery["schema"] = "1"
			case "numeric context":
				f.recovery["execution_id"] = 999
			case "repository":
				f.recovery["repository"] = "fork/project"
			case "source hash":
				f.recovery["source_sha"] = "invalid"
			case "execution":
				f.recovery["execution_id"] = "998"
			case "attempt binding":
				f.recovery["execution_attempt"] = "1"
			case "automation hash":
				f.recovery["automation_sha"] = checkpointSHA
			case "build title":
				f.recovery["build_run_id"] = "124"
			case "uuid":
				f.recovery["submission_id"] = "no-uuid"
			case "archive hash":
				f.recovery["archive_sha256"] = "abc"
			case "team":
				f.recovery["team"] = "short"
			case "version":
				f.recovery["version"] = "../unsafe"
			case "bad digest":
				f.artifacts[1]["digest"] = "sha256:" + strings.Repeat("0", 64)
			case "missing digest":
				f.artifacts[1]["digest"] = ""
			case "expired receipts":
				f.artifacts[1]["expired"] = true
			case "receipt run":
				f.artifacts[1]["workflow_run"].(map[string]any)["id"] = 998
			case "receipt sha":
				f.artifacts[1]["workflow_run"].(map[string]any)["head_sha"] = checkpointSHA
			case "receipt branch":
				f.artifacts[1]["workflow_run"].(map[string]any)["head_branch"] = "feature"
			case "missing receipts":
				f.artifacts[1]["name"] = "other-artifact"
			case "old attempt receipts":
				f.artifacts[1]["name"] = "Apple-receipts-b123-r999-a1-s03"
			case "malformed slot":
				f.artifacts[1]["name"] = "Apple-receipts-b123-r999-a2-sXX"
			case "duplicate slot":
				f.artifacts = append(f.artifacts, map[string]any{"id": 902, "name": f.artifacts[1]["name"]})
			case "missing checkpoint":
				f.artifacts[0]["name"] = "other-artifact"
			case "expired checkpoint":
				f.artifacts[0]["expired"] = true
			case "checkpoint digest":
				f.artifacts[0]["digest"] = "sha256:" + strings.Repeat("f", 64)
			case "checkpoint run":
				f.artifacts[0]["workflow_run"].(map[string]any)["id"] = 998
			case "checkpoint sha":
				f.artifacts[0]["workflow_run"].(map[string]any)["head_sha"] = checkpointSHA
			case "checkpoint slot":
				f.artifacts[0]["name"] = "Apple-notarization-b123-r999-a1-s03"
			case "newer checkpoint":
				f.artifacts = append(f.artifacts, map[string]any{"id": 902, "name": "Apple-notarization-b123-r999-a2-s06"})
			case "newer receipt missing":
				f.artifacts = append(f.artifacts, map[string]any{"id": 902, "name": "Apple-receipts-b123-r999-a2-s06", "expired": false, "digest": f.recovery["checkpoint_digest"], "workflow_run": f.artifacts[0]["workflow_run"]})
			case "newer receipt expired":
				f.artifacts = append(f.artifacts, map[string]any{"id": 902, "name": "Apple-receipts-b123-r999-a2-s06", "expired": true})
			case "missing metadata":
				delete(f.documents, "metadata.json")
			case "metadata binding":
				f.document("metadata.json")["artifact_id"] = "801"
			case "missing stage":
				delete(f.documents, "stage-app.json")
			case "stage binding":
				stage["submission_id"] = "00000000-0000-0000-0000-000000000002"
			case "missing status":
				delete(f.documents, "status-app.json")
			case "status id":
				f.document("status-app.json")["id"] = "00000000-0000-0000-0000-000000000002"
			case "rejected":
				f.document("status-app.json")["status"] = "Rejected"
			case "invalid":
				f.document("status-app.json")["status"] = "Invalid"
			case "none":
				f.recovery["reason"] = "none"
			case "pending phase":
				f.recovery["phase"] = "accepted"
			case "ticket phase":
				f.recovery["reason"] = "ticket-connection-loss"
			case "rejected log", "log mismatch", "log filename", "nonaccepted log":
				f.documents["log-app.json"] = map[string]any{"jobId": f.recovery["submission_id"], "sha256": f.recovery["archive_sha256"], "status": "Accepted"}
				if variant == "rejected log" {
					f.document("log-app.json")["status"] = "Rejected"
				}
				if variant == "nonaccepted log" {
					f.document("log-app.json")["status"] = "In Progress"
				}
				if variant == "log mismatch" {
					f.document("log-app.json")["sha256"] = strings.Repeat("f", 64)
				}
				if variant == "log filename" {
					f.document("log-app.json")["archiveFilename"] = "Other-Product.zip"
				}
			case "rejected receipt":
				f.documents["receipt-app.json"] = map[string]any{"jobId": f.recovery["submission_id"], "sha256": f.recovery["archive_sha256"], "status": "Rejected"}
			case "other stage rejection":
				f.documents["log-dmg.json"] = map[string]any{"jobId": f.recovery["submission_id"], "sha256": f.recovery["archive_sha256"], "status": "Rejected"}
			case "source fork":
				f.source["head_repository"] = map[string]any{"full_name": "fork/project"}
			case "source branch":
				f.source["head_branch"] = "feature"
			case "source event":
				f.source["event"] = "pull_request"
			case "source path":
				f.source["path"] = ".github/workflows/other.yml"
			case "source build":
				f.recovery["build_run_id"] = "124"
			case "source attempt":
				f.source["run_attempt"] = 0
			case "source sha":
				f.source["head_sha"] = strings.Repeat("f", 40)
			case "source number":
				f.source["run_number"] = 46
			case "source active":
				f.source["status"] = "in_progress"
			case "source gate":
				f.gates[0]["conclusion"] = "failure"
			case "missing gate":
				f.gates = nil
			case "gate sha":
				f.gates[0]["head_sha"] = strings.Repeat("f", 40)
			case "gate attempt":
				f.gates[0]["run_attempt"] = 4
			case "app failure":
				f.app[0]["conclusion"] = "failure"
			case "missing original":
				f.originals = nil
			case "expired original":
				f.originals[0]["expired"] = true
			case "original digest":
				f.originals[0]["digest"] = "sha256:" + strings.Repeat("f", 64)
			case "original attempt":
				f.originals[0]["name"] = "Agent-Queue-App-2"
			case "original sha":
				f.originals[0]["workflow_run"].(map[string]any)["head_sha"] = strings.Repeat("f", 40)
			case "original run":
				f.originals[0]["workflow_run"].(map[string]any)["id"] = 124
			case "cancelled":
				f.execution["conclusion"] = "cancelled"
			case "attempt limit":
				f.execution["run_attempt"] = 48
			case "old run":
				f.execution["created_at"] = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
			case "future run":
				f.execution["created_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			case "malformed date":
				f.execution["created_at"] = "yesterday"
			case "failed validation":
				f.jobs[2]["conclusion"] = "failure"
			case "other failed job":
				f.jobs = append(f.jobs, map[string]any{"id": 16, "run_id": 999, "run_attempt": 2, "head_sha": strings.Repeat("b", 40), "status": "completed", "name": "New safety check", "conclusion": "failure"})
			case "cancelled job":
				f.jobs[4]["conclusion"] = "cancelled"
			case "duplicate job":
				f.jobs[0]["name"] = f.jobs[1]["name"]
			case "wrong job sha":
				f.jobs[1]["head_sha"] = checkpointSHA
			case "wrong job attempt":
				f.jobs[1]["run_attempt"] = 3
			case "wrong job run":
				f.jobs[1]["run_id"] = 998
			case "old signing failure":
				f.jobs[1]["run_attempt"] = 1
			default:
				t.Fatal("unhandled variant", variant)
			}
			output, err := f.observe(t)
			if err != nil {
				t.Fatalf("observer should skip unsafe candidate: %v: %s", err, output)
			}
			if variant != "cancelled" && !strings.Contains(output, "Recovery skipped:") {
				t.Fatalf("missing actionable refusal: %s", output)
			}
			f.assertMutations(t, 0)
		})
	}
}

func TestDistributionRecoveryPacesLaterAttempts(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"recent", "future", "before creation", "missing"} {
		t.Run(variant, func(t *testing.T) {
			f := newDistributionRecoveryFixture(t)
			switch variant {
			case "recent":
				f.execution["updated_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			case "future":
				f.execution["updated_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			case "before creation":
				f.execution["updated_at"] = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
			case "missing":
				delete(f.execution, "updated_at")
			}
			output, err := f.observe(t)
			if err != nil || !strings.Contains(output, "Continuation cooldown") {
				t.Fatalf("unpaced continuation: %v: %s", err, output)
			}
			f.assertMutations(t, 0)
		})
	}
}

func TestDistributionRecoveryRejectsUnsafeZIP(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"symlink", "directory", "fifo", "path", "absolute", "duplicate", "newline", "unknown", "large", "malformed JSON", "multiple JSON", "corrupt"} {
		t.Run(variant, func(t *testing.T) {
			f := newDistributionRecoveryFixture(t)
			switch variant {
			case "symlink":
				f.extraEntries = []checkpointEntry{{"toolchain.json", []byte("recovery.json"), os.ModeSymlink | 0o777}}
			case "directory":
				f.extraEntries = []checkpointEntry{{"toolchain.json/", nil, os.ModeDir | 0o700}}
			case "fifo":
				f.extraEntries = []checkpointEntry{{"toolchain.json", nil, os.ModeNamedPipe | 0o600}}
			case "path":
				f.extraEntries = []checkpointEntry{{"../recovery.json", nil, 0o600}}
			case "absolute":
				f.extraEntries = []checkpointEntry{{"/recovery.json", nil, 0o600}}
			case "duplicate":
				f.extraEntries = []checkpointEntry{{"recovery.json", []byte("{}"), 0o600}}
			case "newline":
				f.extraEntries = []checkpointEntry{{"toolchain.json\nrecovery.json", nil, 0o600}}
			case "unknown":
				f.extraEntries = []checkpointEntry{{"private.p8", nil, 0o600}}
			case "large":
				f.documents["toolchain.json"] = map[string]any{"value": strings.Repeat("x", 1048577)}
			case "malformed JSON":
				f.archive = checkpointZIP(t, checkpointEntry{"recovery.json", []byte("{"), 0o600}, checkpointEntry{"metadata.json", []byte("{}"), 0o600})
			case "multiple JSON":
				f.archive = checkpointZIP(t, checkpointEntry{"recovery.json", []byte("{} {}"), 0o600}, checkpointEntry{"metadata.json", []byte("{}"), 0o600})
			case "corrupt":
				f.archive = []byte("not a ZIP")
			}
			output, err := f.observe(t)
			if err != nil || !strings.Contains(output, "Recovery skipped:") {
				t.Fatalf("unsafe ZIP was not skipped: %v: %s", err, output)
			}
			f.assertMutations(t, 0)
		})
	}
}

func (f *distributionRecoveryFixture) ticket() {
	f.recovery["reason"], f.recovery["phase"] = "ticket-connection-loss", "accepted"
	f.document("stage-app.json")["phase"] = "accepted"
	f.document("status-app.json")["status"] = "Accepted"
	for _, name := range []string{"receipt-app.json", "log-app.json"} {
		f.documents[name] = map[string]any{"jobId": f.recovery["submission_id"], "sha256": f.recovery["archive_sha256"], "status": "Accepted"}
	}
}

func TestDistributionRecoveryTicketAndDeliveryOnlyRetries(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"ticket accepted", "ticket finished", "ticket legacy filename", "ticket bound filename", "publish", "homebrew", "retained signing", "publish validation failed", "homebrew signing failed", "missing intel", "delivery other failure", "legacy delivery", "ticket missing acceptance"} {
		t.Run(variant, func(t *testing.T) {
			f := newDistributionRecoveryFixture(t)
			f.ticket()
			want := 1
			switch variant {
			case "ticket finished":
				f.recovery["phase"] = "finished"
				f.document("stage-app.json")["phase"] = "finished"
			case "ticket missing acceptance":
				delete(f.documents, "receipt-app.json")
				want = 0
			case "ticket legacy filename":
				f.document("log-app.json")["archiveFilename"] = "Agent-Queue-0.1.0-universal.zip"
			case "ticket bound filename":
				f.document("log-app.json")["archiveFilename"] = "Agent-Queue-0.1.0-build-45-1111111-universal.zip"
			}
			if !strings.HasPrefix(variant, "ticket") {
				f.recovery["reason"] = "none"
				for _, job := range f.jobs {
					job["conclusion"] = "success"
				}
				f.jobs[4]["conclusion"] = "failure"
				if strings.HasPrefix(variant, "homebrew") {
					f.jobs[4]["conclusion"] = "success"
					f.jobs[5]["conclusion"] = "failure"
				}
				switch variant {
				case "publish validation failed":
					f.jobs[2]["conclusion"] = "failure"
					want = 0
				case "homebrew signing failed":
					f.jobs[1]["conclusion"] = "failure"
					want = 0
				case "missing intel":
					f.jobs[3]["name"] = "Different acceptance"
					want = 0
				case "delivery other failure":
					f.jobs[0]["conclusion"] = "failure"
					want = 0
				case "legacy delivery":
					f.artifacts[1]["name"] = "legacy-receipts"
					want = 0
				case "retained signing":
					f.execution["run_attempt"] = 3
					f.jobs[4]["run_attempt"] = 3
				}
			}
			if output, err := f.observe(t); err != nil {
				t.Fatalf("observer: %v: %s", err, output)
			}
			f.assertMutations(t, want)
		})
	}
}

func TestDistributionRecoveryPermitsFinalBoundedAttempt(t *testing.T) {
	t.Parallel()
	f := newDistributionRecoveryFixture(t)
	f.execution["run_attempt"] = 47
	f.recovery["execution_attempt"] = "47"
	f.document("stage-app.json")["prepared_attempt"] = "47"
	f.document("status-app.json")["execution_attempt"] = "47"
	for _, job := range f.jobs {
		if job["run_attempt"] == 2 {
			job["run_attempt"] = 47
		}
	}
	for _, artifact := range f.artifacts {
		artifact["name"] = strings.ReplaceAll(artifact["name"].(string), "-a2-", "-a47-")
	}
	output, err := f.observe(t)
	if err != nil || !strings.Contains(output, "next_attempt=48") {
		t.Fatalf("last permitted continuation: %v: %s", err, output)
	}
	f.assertMutations(t, 1)
}

func TestDistributionRecoveryOrdersSlotsNumericallyAcrossArtifactIDs(t *testing.T) {
	t.Parallel()
	f := newDistributionRecoveryFixture(t)
	// A high artifact ID must not displace the later stage slot.
	f.artifacts = append(f.artifacts,
		map[string]any{"id": 990, "name": "Apple-receipts-b123-r999-a2-s02", "expired": true},
		map[string]any{"id": 991, "name": "Apple-notarization-b123-r999-a2-s02", "expired": true})
	output, err := f.observe(t)
	if err != nil || !strings.Contains(output, "Recovery requested:") {
		t.Fatalf("numeric slot selection: %v: %s", err, output)
	}
	f.assertMutations(t, 1)
}

func TestDistributionRecoveryFindsSuccessfulSiblingOnSecondPage(t *testing.T) {
	t.Parallel()
	f := newDistributionRecoveryFixture(t)
	f.write(t)
	sibling := distributionRecoveryCandidate(f.checkpointFixture, 998, 123).execution
	sibling["conclusion"] = "success"
	f.writeJSON(t, "runs-1.json", map[string]any{"total_count": 2, "workflow_runs": []map[string]any{f.execution}})
	f.writeJSON(t, "runs-2.json", map[string]any{"total_count": 2, "workflow_runs": []map[string]any{sibling}})
	if output, err := f.run(t, nil); err != nil {
		t.Fatalf("paginated guard: %v: %s", err, output)
	}
	f.assertMutations(t, 0)
}

func TestDistributionRecoveryFiltersAndGroupGuards(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"fork", "branch", "path", "event", "title", "active", "success", "active sibling", "success sibling", "older failure", "new active", "new success", "changed attempt"} {
		t.Run(variant, func(t *testing.T) {
			f := newDistributionRecoveryFixture(t)
			want := 0
			switch variant {
			case "fork":
				f.execution["head_repository"] = map[string]any{"full_name": "fork/project"}
			case "branch":
				f.execution["head_branch"] = "feature"
			case "path":
				f.execution["path"] = ".github/workflows/other.yml"
			case "event":
				f.execution["event"] = "pull_request"
			case "title":
				f.execution["display_title"] = "Distribute build 123 injected"
			case "active":
				f.execution["status"] = "in_progress"
			case "success":
				f.execution["conclusion"] = "success"
			case "active sibling", "success sibling", "older failure":
				sibling := distributionRecoveryCandidate(f.checkpointFixture, 998, 123).execution
				if variant == "active sibling" {
					sibling["status"] = "queued"
				}
				if variant == "success sibling" {
					sibling["conclusion"] = "success"
				}
				if variant == "older failure" {
					want = 1
				}
				f.otherRuns = append(f.otherRuns, sibling)
			case "new active", "new success", "changed attempt":
				fresh := distributionRecoveryCandidate(f.checkpointFixture, 999, 123).execution
				if variant == "new active" {
					fresh["status"] = "queued"
				}
				if variant == "new success" {
					fresh["conclusion"] = "success"
				}
				if variant == "changed attempt" {
					fresh["run_attempt"] = 3
				}
				f.writeJSON(t, "fresh-1.json", map[string]any{"total_count": 1, "workflow_runs": []map[string]any{fresh}})
			}
			if output, err := f.observe(t); err != nil {
				t.Fatalf("observer: %v: %s", err, output)
			}
			f.assertMutations(t, want)
		})
	}
}

func TestDistributionRecoveryPaginationAndFiveCandidateBound(t *testing.T) {
	t.Parallel()
	for _, result := range []string{"0", "1"} {
		t.Run("POST result "+result, func(t *testing.T) {
			testDistributionRecoveryCandidateBudget(t, result)
		})
	}
}

func testDistributionRecoveryCandidateBudget(t *testing.T, postResult string) {
	t.Helper()
	f := newDistributionRecoveryFixture(t)
	runs := []map[string]any{}
	candidates := []*distributionRecoveryFixture{}
	for i := 0; i < 6; i++ {
		candidate := distributionRecoveryCandidate(f.checkpointFixture, 999+i, 123+i)
		candidate.write(t)
		candidates = append(candidates, candidate)
		runs = append(runs, candidate.execution)
	}
	// A second page is required, including during the pre-mutation recheck.
	f.writeJSON(t, "runs-1.json", map[string]any{"total_count": 6, "workflow_runs": runs[:2]})
	f.writeJSON(t, "runs-2.json", map[string]any{"total_count": 6, "workflow_runs": runs[2:]})
	// Each candidate has its own archive binding, even though artifact IDs are
	// deliberately shared by this API fixture. Route the ZIP by the latest read.
	testTool(t, filepath.Join(f.root, "tools"), "gh", `printf '%s\n' "$*" >> "$CHECKPOINT_TEST_ROOT/calls"
[ "$1" = api ] || exit 99
shift
if [ "$1" = --method ]; then
 [ "$2" = POST ] && [ "${3##*/}" = rerun-failed-jobs ] || exit 99
 exit "${RECOVERY_POST_RESULT:-0}"
fi
case "$1" in
 */workflows/distribution.yml/runs\?*) page=${1##*=};cat "$CHECKPOINT_TEST_ROOT/runs-$page.json";;
 */artifacts/901/zip) id=$(cat "$CHECKPOINT_TEST_ROOT/selected");cat "$CHECKPOINT_TEST_ROOT/receipt-$id.zip";;
 */runs/*)
 tail=${1#*/runs/};id=${tail%%/*}
 case "$tail" in
 */artifacts\?*) printf '%s' "$id" > "$CHECKPOINT_TEST_ROOT/selected";cat "$CHECKPOINT_TEST_ROOT/artifacts-$id.json";;
 */attempts/*/jobs\?*) cat "$CHECKPOINT_TEST_ROOT/app-$id.json";;
 */jobs\?*) cat "$CHECKPOINT_TEST_ROOT/jobs-$id.json";;
 *) cat "$CHECKPOINT_TEST_ROOT/run-$id.json";;
 esac;;
 *) exit 98;;
esac
`)
	// Reuse the exact executions advertised by the inventory. Recreating them
	// would advance created_at/updated_at across a wall-clock second and rightly
	// trip the observer's immutable-execution check under concurrent test load.
	for i, candidate := range candidates {
		candidate.write(t)
		data, err := os.ReadFile(filepath.Join(f.root, "artifact-901.zip"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.root, fmt.Sprintf("receipt-%d.zip", 999+i)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.writeJSON(t, "runs-1.json", map[string]any{"total_count": 6, "workflow_runs": runs[:2]})
	output, err := f.run(t, []string{"RECOVERY_POST_RESULT=" + postResult})
	successes := "5"
	if postResult != "0" {
		successes = "0"
	}
	if err != nil || !strings.Contains(output, successes+" same-run retries requested") {
		t.Fatalf("bounded recovery: %v: %s", err, output)
	}
	f.assertMutations(t, 5)
	calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	if !strings.Contains(string(calls), "per_page=100&page=2") {
		t.Fatal("did not read paginated runs")
	}
}

func TestDistributionRecoveryReportsProviderRefusal(t *testing.T) {
	t.Parallel()
	f := newDistributionRecoveryFixture(t)
	output, err := f.observe(t, "RECOVERY_POST_RESULT=1")
	if err != nil || !strings.Contains(output, "GitHub refused failed-job recovery") || strings.Contains(output, "Recovery requested:") {
		t.Fatalf("provider refusal was hidden: %v: %s", err, output)
	}
	f.assertMutations(t, 1)
}
