package tooling

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newDistributionCheckpointFixture(t *testing.T) checkpointFixture {
	t.Helper()
	f := newCheckpointFixture(t)
	testTool(t, filepath.Join(f.root, "tools"), "gh", `printf '%s\n' "$*" >> "$CHECKPOINT_TEST_ROOT/calls"
[ "$1" = api ] || exit 99
case "$2" in
 repos/owner/project/actions/workflows/distribution.yml/runs\?branch=main\&per_page=100\&page=*) page=${2##*=};cat "$CHECKPOINT_TEST_ROOT/runs-$page.json";;
 repos/owner/project/actions/runs/123) cat "$CHECKPOINT_TEST_ROOT/run.json";;
 repos/owner/project/actions/runs/123/jobs\?filter=all\&per_page=100\&page=*) cat "$CHECKPOINT_TEST_ROOT/legacy-jobs.json";;
 repos/owner/project/actions/runs/*/jobs\?filter=all\&per_page=100\&page=*) id=${2#*/runs/};id=${id%%/*};cat "$CHECKPOINT_TEST_ROOT/jobs-$id.json";;
 repos/owner/project/actions/runs/*/artifacts\?per_page=100\&page=*) id=${2#*/runs/};id=${id%%/*}; page=${2##*=};cat "$CHECKPOINT_TEST_ROOT/inventory-$id-$page.json";;
 repos/owner/project/actions/runs/*) id=${2##*/};cat "$CHECKPOINT_TEST_ROOT/execution-$id.json";;
 repos/owner/project/actions/artifacts/*/zip) id=${2%/zip};id=${id##*/};cat "$CHECKPOINT_TEST_ROOT/artifact-$id.zip";;
 *) exit 99;;
esac
`)
	f.env = append(f.env, "GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_RUN_ID=999", "GITHUB_RUN_NUMBER=101", "GITHUB_RUN_ATTEMPT=1", "GITHUB_SHA="+strings.Repeat("b", 40), "BUILD_RUN_ID=123", "BUILD_RUN_NUMBER=45", "BUILD_RUN_ATTEMPT=1", "BUILD_SHA="+checkpointSHA, "BUILD_ARTIFACT_ID=800", "BUILD_ARTIFACT_DIGEST=sha256:"+strings.Repeat("c", 64))
	f.changeJSON(t, "run.json", func(v map[string]any) { v["run_attempt"] = 1; v["status"] = "completed"; v["conclusion"] = "failure" })
	f.writeJSON(t, "legacy-jobs.json", map[string]any{"total_count": 0, "jobs": []any{}})
	f.writeJSON(t, "jobs-999.json", map[string]any{"total_count": 0, "jobs": []any{}})
	f.writeJSON(t, "jobs-222.json", map[string]any{"total_count": 1, "jobs": []any{map[string]any{"id": 22, "run_id": 222, "head_sha": strings.Repeat("d", 40), "status": "completed", "conclusion": "success", "name": "Sign and notarize main installer", "run_attempt": 1, "steps": []any{map[string]any{"name": "Notarize the distribution disk image", "status": "completed", "conclusion": "success"}}}}})
	for _, id := range []string{"123", "222", "999"} {
		f.writeJSON(t, "inventory-"+id+"-1.json", map[string]any{"total_count": 0, "artifacts": []any{}})
	}
	current := map[string]any{"id": 999, "path": ".github/workflows/distribution.yml", "head_branch": "main", "head_sha": strings.Repeat("b", 40), "event": "workflow_dispatch", "display_title": "Distribute build 123", "run_attempt": 1, "status": "in_progress", "repository": map[string]any{"full_name": "owner/project"}, "head_repository": map[string]any{"full_name": "owner/project"}}
	previous := map[string]any{}
	for k, v := range current {
		previous[k] = v
	}
	previous["id"] = 222
	previous["status"] = "completed"
	previous["head_sha"] = strings.Repeat("d", 40)
	f.writeJSON(t, "execution-999.json", current)
	f.writeJSON(t, "execution-222.json", previous)
	f.writeJSON(t, "runs-1.json", map[string]any{"total_count": 2, "workflow_runs": []any{current, previous}})
	return f
}

func (f checkpointFixture) distributionArtifact(t *testing.T, id int, run string, attempt, slot int) map[string]any {
	t.Helper()
	artifact := f.artifact(t, id, attempt, slot)
	artifact["name"] = fmt.Sprintf("Apple-notarization-b123-r%s-a%d-s%02d", run, attempt, slot)
	sha := strings.Repeat("d", 40)
	if run == "999" {
		sha = strings.Repeat("b", 40)
	}
	runNumber, err := strconv.Atoi(run)
	if err != nil {
		t.Fatal(err)
	}
	artifact["workflow_run"] = map[string]any{"id": runNumber, "head_branch": "main", "head_sha": sha}
	return artifact
}

func TestDistributionCheckpointRecoversAcrossAutomationRuns(t *testing.T) {
	f := newDistributionCheckpointFixture(t)
	latest := f.distributionArtifact(t, 2010, "222", 1, 9)
	f.writeJSON(t, "inventory-222-1.json", map[string]any{"total_count": 2, "artifacts": []any{f.distributionArtifact(t, 2001, "222", 1, 1), latest}})
	output, err := f.run(t, nil, "restore", f.state)
	if err != nil {
		t.Fatalf("cross-run restore: %v: %s", err, output)
	}
	body, err := os.ReadFile(filepath.Join(f.state, "executable"))
	if err != nil || string(body) != "checkpoint 2010" {
		t.Fatalf("wrong exact checkpoint: %s (%v)", body, err)
	}
	calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	if strings.Count(string(calls), "/zip") != 1 || !strings.Contains(string(calls), "/artifacts/2010/zip") {
		t.Fatalf("wrong checkpoint download: %s", calls)
	}
}

func TestDistributionCheckpointPreservesLegacyCIRecovery(t *testing.T) {
	f := newDistributionCheckpointFixture(t)
	// No prior distribution execution; the source CI failed after Apple submission.
	f.changeJSON(t, "runs-1.json", func(v map[string]any) { v["workflow_runs"] = v["workflow_runs"].([]any)[:1]; v["total_count"] = 1 })
	f.writeJSON(t, "legacy-jobs.json", map[string]any{"total_count": 1, "jobs": []any{map[string]any{"id": 1, "run_id": 123, "head_sha": checkpointSHA, "status": "completed", "name": "Sign and notarize main installer", "run_attempt": 1, "conclusion": "failure"}}})
	f.writeJSON(t, "inventory-123-1.json", map[string]any{"total_count": 1, "artifacts": []any{f.artifact(t, 3002, 1, 2)}})
	output, err := f.run(t, nil, "restore", f.state)
	if err != nil {
		t.Fatalf("legacy restore: %v: %s", err, output)
	}
	body, _ := os.ReadFile(filepath.Join(f.state, "executable"))
	if string(body) != "checkpoint 3002" {
		t.Fatalf("legacy checkpoint ignored: %s", body)
	}
}

func TestDistributionCheckpointFailsClosedAcrossExecutions(t *testing.T) {
	for _, variant := range []string{"missing newest run", "missing latest attempt", "expired newest", "wrong repo", "wrong SHA", "wrong workflow", "wrong branch", "wrong source build", "wrong execution name", "wrong artifact SHA", "future attempt", "missing current run", "corrupt digest", "legacy missing", "legacy expired"} {
		t.Run(variant, func(t *testing.T) {
			f := newDistributionCheckpointFixture(t)
			artifact := f.distributionArtifact(t, 2009, "222", 1, 9)
			reason := "checkpoint"
			switch variant {
			case "missing newest run":
				artifact = nil
			case "missing latest attempt":
				f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["run_attempt"] = 2 })
				f.changeJSON(t, "jobs-222.json", func(v map[string]any) {
					job := v["jobs"].([]any)[0].(map[string]any)
					newer := map[string]any{}
					for k, value := range job {
						newer[k] = value
					}
					newer["id"] = 23
					newer["run_attempt"] = 2
					v["jobs"] = append(v["jobs"].([]any), newer)
					v["total_count"] = 2
				})
			case "expired newest":
				artifact["expired"] = true
			case "wrong repo":
				f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["head_repository"] = map[string]any{"full_name": "fork/project"} })
				reason = "provenance"
			case "wrong SHA":
				f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["head_sha"] = strings.Repeat("e", 40) })
				reason = "provenance"
			case "wrong workflow":
				f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["path"] = ".github/workflows/ci.yml" })
				reason = "provenance"
			case "wrong branch":
				f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["head_branch"] = "feature" })
				reason = "provenance"
			case "wrong source build":
				artifact["name"] = "Apple-notarization-b124-r222-a1-s09"
				reason = "provenance"
			case "wrong execution name":
				artifact["name"] = "Apple-notarization-b123-r333-a1-s09"
				reason = "provenance"
			case "wrong artifact SHA":
				artifact["workflow_run"].(map[string]any)["head_sha"] = strings.Repeat("e", 40)
				reason = "provenance"
			case "future attempt":
				artifact["name"] = "Apple-notarization-b123-r222-a2-s09"
				reason = "provenance"
			case "missing current run":
				f.changeJSON(t, "runs-1.json", func(v map[string]any) { v["workflow_runs"] = v["workflow_runs"].([]any)[1:]; v["total_count"] = 1 })
				reason = "Current distribution"
			case "corrupt digest":
				artifact["digest"] = "sha256:" + strings.Repeat("0", 64)
				reason = "digest mismatch"
			case "legacy missing", "legacy expired":
				f.writeJSON(t, "legacy-jobs.json", map[string]any{"total_count": 1, "jobs": []any{map[string]any{"id": 1, "run_id": 123, "head_sha": checkpointSHA, "status": "completed", "name": "Sign and notarize main installer", "run_attempt": 1, "conclusion": "failure"}}})
				if variant == "legacy expired" {
					legacy := f.artifact(t, 1002, 1, 2)
					legacy["expired"] = true
					f.writeJSON(t, "inventory-123-1.json", map[string]any{"total_count": 1, "artifacts": []any{legacy}})
				}
			}
			if artifact != nil {
				f.writeJSON(t, "inventory-222-1.json", map[string]any{"total_count": 1, "artifacts": []any{artifact}})
			}
			f.reject(t, reason)
		})
	}
}

func TestDistributionCheckpointFirstExecutionAndPaginatedDiscovery(t *testing.T) {
	f := newDistributionCheckpointFixture(t)
	// Retain only this first distribution execution and no legacy signing job.
	f.changeJSON(t, "runs-1.json", func(v map[string]any) { v["workflow_runs"] = v["workflow_runs"].([]any)[:1]; v["total_count"] = 1 })
	output, err := f.run(t, nil, "restore", f.state)
	if err != nil {
		t.Fatalf("first execution: %v: %s", err, output)
	}
	result, _ := os.ReadFile(filepath.Join(f.root, "output"))
	if string(result) != "restored=false\n" {
		t.Fatalf("wrong first-execution decision: %s", result)
	}
	f = newDistributionCheckpointFixture(t)
	f.changeJSON(t, "runs-1.json", func(v map[string]any) {
		runs := v["workflow_runs"].([]any)
		f.writeJSON(t, "runs-2.json", map[string]any{"total_count": 2, "workflow_runs": runs[1:]})
		v["workflow_runs"] = runs[:1]
	})
	f.writeJSON(t, "inventory-222-1.json", map[string]any{"total_count": 1, "artifacts": []any{f.distributionArtifact(t, 2009, "222", 1, 9)}})
	output, err = f.run(t, nil, "restore", f.state)
	if err != nil {
		t.Fatalf("paginated discovery: %v: %s", err, output)
	}
	calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	if !strings.Contains(string(calls), "branch=main&per_page=100&page=2") {
		t.Fatal("distribution discovery did not paginate")
	}
}

func TestDistributionCheckpointRefusesDeletedTrailingStageState(t *testing.T) {
	for _, variant := range []string{"finished checkpoint deleted", "prepared and submitted checkpoints deleted"} {
		t.Run(variant, func(t *testing.T) {
			f := newDistributionCheckpointFixture(t)
			slot := 8
			if variant == "prepared and submitted checkpoints deleted" {
				slot = 6
				f.changeJSON(t, "jobs-222.json", func(v map[string]any) {
					job := v["jobs"].([]any)[0].(map[string]any)
					job["steps"].([]any)[0].(map[string]any)["conclusion"] = "failure"
				})
			}
			f.writeJSON(t, "inventory-222-1.json", map[string]any{"total_count": 1, "artifacts": []any{f.distributionArtifact(t, 2008, "222", 1, slot)}})
			f.reject(t, "Newest stage checkpoint is missing")
			calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
			if strings.Contains(string(calls), "/zip") {
				t.Fatal("deleted newest state fell back to older state")
			}
		})
	}
}

func TestDistributionCheckpointUsesRealSigningProgress(t *testing.T) {
	for _, variant := range []string{"skipped after source failure", "acceptance-only retry", "publication-only retry", "current acceptance retry", "queued cancellation", "newer queued first execution", "cancelled before signing starts", "cloned successful signer"} {
		t.Run(variant, func(t *testing.T) {
			f := newDistributionCheckpointFixture(t)
			extra := []string{}
			wantRestored := variant == "acceptance-only retry" || variant == "publication-only retry" || variant == "cloned successful signer"
			switch variant {
			case "cloned successful signer":
				f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["run_attempt"] = 2 })
				distributionCloneSigningJob(t, f, "")
				f.writeJSON(t, "inventory-222-1.json", map[string]any{"total_count": 1, "artifacts": []any{f.distributionArtifact(t, 2009, "222", 1, 9)}})
			case "skipped after source failure":
				f.changeJSON(t, "jobs-222.json", func(v map[string]any) {
					job := v["jobs"].([]any)[0].(map[string]any)
					job["conclusion"] = "skipped"
					job["steps"] = []any{}
				})
			case "cancelled before signing starts":
				f.changeJSON(t, "jobs-222.json", func(v map[string]any) {
					job := v["jobs"].([]any)[0].(map[string]any)
					job["conclusion"] = "cancelled"
					job["started_at"] = nil
					job["steps"] = []any{}
				})
			case "newer queued first execution":
				f.changeJSON(t, "runs-1.json", func(v map[string]any) {
					run := v["workflow_runs"].([]any)[1].(map[string]any)
					run["id"] = 1000
					run["status"] = "queued"
					run["run_started_at"] = nil
				})
				f.changeJSON(t, "execution-222.json", func(v map[string]any) {
					v["id"] = 1000
					v["status"] = "queued"
					v["run_started_at"] = nil
					f.writeJSON(t, "execution-1000.json", v)
				})
				f.writeJSON(t, "jobs-1000.json", map[string]any{"total_count": 0, "jobs": []any{}})
				f.writeJSON(t, "inventory-1000-1.json", map[string]any{"total_count": 0, "artifacts": []any{}})
			case "queued cancellation":
				f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["conclusion"] = "cancelled"; v["run_started_at"] = nil })
				f.writeJSON(t, "jobs-222.json", map[string]any{"total_count": 0, "jobs": []any{}})
			case "current acceptance retry":
				// This run previously failed source validation and never entered signing.
				f.changeJSON(t, "runs-1.json", func(v map[string]any) { v["workflow_runs"] = v["workflow_runs"].([]any)[:1]; v["total_count"] = 1 })
				f.changeJSON(t, "execution-999.json", func(v map[string]any) { v["run_attempt"] = 2 })
				extra = []string{"GITHUB_RUN_ATTEMPT=2"}
				f.writeJSON(t, "jobs-999.json", map[string]any{"total_count": 1, "jobs": []any{map[string]any{
					"id": 99, "run_id": 999, "head_sha": strings.Repeat("b", 40), "name": "Sign and notarize main installer", "run_attempt": 1, "status": "completed", "conclusion": "skipped", "steps": []any{}}}})
			default:
				f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["run_attempt"] = 2 })
				name := "Notarized distribution (macos-15)"
				if variant == "publication-only retry" {
					name = "Publish main installer"
				}
				f.changeJSON(t, "jobs-222.json", func(v map[string]any) {
					v["jobs"] = append(v["jobs"].([]any), map[string]any{"id": 23, "run_id": 222, "head_sha": strings.Repeat("d", 40), "name": name, "run_attempt": 2, "status": "completed", "conclusion": "success"})
					v["total_count"] = 2
				})
				f.writeJSON(t, "inventory-222-1.json", map[string]any{"total_count": 1, "artifacts": []any{f.distributionArtifact(t, 2009, "222", 1, 9)}})
			}
			output, err := f.run(t, extra, "restore", f.state)
			if err != nil {
				t.Fatalf("proven signing progress rejected: %v: %s", err, output)
			}
			result, _ := os.ReadFile(filepath.Join(f.root, "output"))
			if !strings.Contains(string(result), fmt.Sprintf("restored=%t\n", wantRestored)) {
				t.Fatalf("wrong recovery decision: %s", result)
			}
		})
	}
}

func TestDistributionCheckpointRejectsUnknownSigningHistory(t *testing.T) {
	for _, variant := range []string{"missing jobs", "unknown retry", "cancelled without start evidence", "cancelled signing started", "wrong jobs SHA", "future jobs attempt", "retry after failed signing"} {
		t.Run(variant, func(t *testing.T) {
			f := newDistributionCheckpointFixture(t)
			switch variant {
			case "missing jobs", "cancelled without start evidence":
				f.writeJSON(t, "jobs-222.json", map[string]any{"total_count": 0, "jobs": []any{}})
				if variant == "cancelled without start evidence" {
					f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["conclusion"] = "cancelled" })
				}
			case "unknown retry", "retry after failed signing":
				f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["run_attempt"] = 2 })
				f.changeJSON(t, "jobs-222.json", func(v map[string]any) {
					if variant == "retry after failed signing" {
						v["jobs"].([]any)[0].(map[string]any)["conclusion"] = "failure"
						v["jobs"] = append(v["jobs"].([]any), map[string]any{"id": 23, "run_id": 222, "head_sha": strings.Repeat("d", 40), "name": "Publish main installer", "run_attempt": 2, "status": "completed", "conclusion": "success"})
						v["total_count"] = 2
					}
				})
			default:
				f.changeJSON(t, "jobs-222.json", func(v map[string]any) {
					job := v["jobs"].([]any)[0].(map[string]any)
					switch variant {
					case "wrong jobs SHA":
						job["head_sha"] = checkpointSHA
					case "future jobs attempt":
						job["run_attempt"] = 2
					case "cancelled signing started":
						job["conclusion"] = "cancelled"
						job["started_at"] = "2026-10-22T12:00:00Z"
					}
				})
			}
			// Valid older state must not hide unknown newer signing history.
			if variant != "cancelled signing started" {
				f.writeJSON(t, "inventory-222-1.json", map[string]any{"total_count": 1, "artifacts": []any{f.distributionArtifact(t, 2009, "222", 1, 9)}})
			}
			output, err := f.run(t, nil, "restore", f.state)
			if err == nil {
				t.Fatalf("unknown signing history accepted: %s", output)
			}
			calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
			if strings.Contains(string(calls), "/zip") {
				t.Fatalf("unknown history downloaded fallback: %s", calls)
			}
		})
	}
}

// GitHub gives reused successful jobs new IDs and run_attempt values, retaining
// the actual execution timestamps and step evidence from the original attempt.
func distributionCloneSigningJob(t *testing.T, f checkpointFixture, change string) {
	t.Helper()
	f.changeJSON(t, "jobs-222.json", func(v map[string]any) {
		job := v["jobs"].([]any)[0].(map[string]any)
		job["started_at"] = "2026-10-22T13:25:36Z"
		job["completed_at"] = "2026-10-22T13:32:09Z"
		step := job["steps"].([]any)[0].(map[string]any)
		step["number"] = 1
		step["started_at"] = "2026-10-22T13:30:00Z"
		step["completed_at"] = "2026-10-22T13:32:00Z"
		clone := map[string]any{}
		for k, value := range job {
			clone[k] = value
		}
		clone["id"] = 23
		clone["run_attempt"] = 2
		clone["check_run_url"] = "https://api.github.com/repos/owner/project/check-runs/23"
		switch change {
		case "different timestamps":
			clone["started_at"] = "2026-10-22T13:49:13Z"
			clone["completed_at"] = "2026-10-22T13:55:00Z"
		case "missing timestamp":
			delete(clone, "started_at")
		case "different steps":
			clone["steps"] = []any{map[string]any{"number": 1, "name": "Notarize the distribution disk image", "status": "completed", "conclusion": "success", "started_at": "2026-10-22T13:49:14Z", "completed_at": "2026-10-22T13:54:00Z"}}
		case "missing steps":
			delete(clone, "steps")
		}
		v["jobs"] = append(v["jobs"].([]any), clone)
		v["total_count"] = 2
	})
}

func TestDistributionCheckpointRequiresStateForNewOrUnprovenCopiedSigning(t *testing.T) {
	for _, change := range []string{"different timestamps", "missing timestamp", "different steps", "missing steps", "missing original execution"} {
		t.Run(change, func(t *testing.T) {
			f := newDistributionCheckpointFixture(t)
			f.changeJSON(t, "execution-222.json", func(v map[string]any) { v["run_attempt"] = 2 })
			distributionCloneSigningJob(t, f, change)
			if change == "missing original execution" {
				f.changeJSON(t, "jobs-222.json", func(v map[string]any) { v["jobs"] = v["jobs"].([]any)[1:]; v["total_count"] = 1 })
			}
			f.writeJSON(t, "inventory-222-1.json", map[string]any{"total_count": 1, "artifacts": []any{f.distributionArtifact(t, 2009, "222", 1, 9)}})
			f.reject(t, "checkpoint")
		})
	}
}

func TestDistributionCheckpointRecognizesClonedLegacySigningExecution(t *testing.T) {
	f := newDistributionCheckpointFixture(t)
	distributionCloneSigningJob(t, f, "")
	f.changeJSON(t, "jobs-222.json", func(v map[string]any) {
		for _, raw := range v["jobs"].([]any) {
			job := raw.(map[string]any)
			job["run_id"] = 123
			job["head_sha"] = checkpointSHA
		}
		f.writeJSON(t, "legacy-jobs.json", v)
	})
	f.changeJSON(t, "run.json", func(v map[string]any) { v["run_attempt"] = 2 })
	f.changeJSON(t, "runs-1.json", func(v map[string]any) { v["workflow_runs"] = v["workflow_runs"].([]any)[:1]; v["total_count"] = 1 })
	f.writeJSON(t, "inventory-123-1.json", map[string]any{"total_count": 1, "artifacts": []any{f.artifact(t, 3009, 1, 9)}})
	output, err := f.run(t, nil, "restore", f.state)
	if err != nil {
		t.Fatalf("cloned legacy signer required invented checkpoint: %v: %s", err, output)
	}
	body, _ := os.ReadFile(filepath.Join(f.state, "executable"))
	if string(body) != "checkpoint 3009" {
		t.Fatalf("lost exact legacy checkpoint: %s", body)
	}
}

func TestDistributionCheckpointDoesNotSkipQueuedRerunHistory(t *testing.T) {
	f := newDistributionCheckpointFixture(t)
	f.changeJSON(t, "runs-1.json", func(v map[string]any) {
		// A newer execution queued for a retry can already have signed before.
		run := v["workflow_runs"].([]any)[1].(map[string]any)
		run["id"] = 1000
		run["run_attempt"] = 2
		run["status"] = "queued"
	})
	f.changeJSON(t, "execution-222.json", func(v map[string]any) {
		v["id"] = 1000
		v["run_attempt"] = 2
		v["status"] = "queued"
		f.writeJSON(t, "execution-1000.json", v)
	})
	f.changeJSON(t, "jobs-222.json", func(v map[string]any) {
		v["jobs"].([]any)[0].(map[string]any)["run_id"] = 1000
		f.writeJSON(t, "jobs-1000.json", v)
	})
	f.writeJSON(t, "inventory-1000-1.json", map[string]any{"total_count": 0, "artifacts": []any{}})
	f.reject(t, "checkpoint")
}
