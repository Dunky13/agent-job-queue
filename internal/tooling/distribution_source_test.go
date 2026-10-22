package tooling

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newDistributionSourceFixture(t *testing.T, event string) checkpointFixture {
	t.Helper()
	f := newCheckpointFixture(t)
	f.script, _ = filepath.Abs("../../scripts/distribution-source.sh")
	tools := filepath.Join(f.root, "tools")
	testTool(t, tools, "gh", `printf '%s\n' "$*" >> "$CHECKPOINT_TEST_ROOT/calls"
[ "$1" = api ] || exit 99
case "$2" in
 repos/owner/project/actions/runs/123) cat "$CHECKPOINT_TEST_ROOT/run.json";;
 repos/owner/project/actions/runs/123/artifacts\?per_page=100\&page=*) page=${2##*=}; cat "$CHECKPOINT_TEST_ROOT/page-$page.json";;
 repos/owner/project/actions/runs/123/jobs\?filter=latest\&per_page=100\&page=*) cat "$CHECKPOINT_TEST_ROOT/effective-jobs.json";;
 repos/owner/project/actions/runs/123/attempts/*/jobs\?per_page=100\&page=*) cat "$CHECKPOINT_TEST_ROOT/jobs.json";;
 repos/owner/project/actions/artifacts/800/zip) printf 'fixture archive bytes';;
 *) exit 99;;
esac
`)
	testTool(t, tools, "queue-tool", `printf '%s\n' "$*" >> "$CHECKPOINT_TEST_ROOT/verifier-calls"
[ "$1" = verify-build-artifact ] || exit 98
printf '0.1.0\n'
`)
	f.env = append(f.env, "GITHUB_EVENT_NAME="+event, "GITHUB_RUN_ID=999", "GITHUB_RUN_NUMBER=101", "GITHUB_RUN_ATTEMPT=1", "GITHUB_SHA="+strings.Repeat("b", 40), "INPUT_BUILD_RUN_ID=123", "QUEUE_NOTARY_TOOL="+filepath.Join(tools, "queue-tool"), "GITHUB_EVENT_PATH="+filepath.Join(f.root, "event.json"))
	run := map[string]any{"id": 123, "run_number": 45, "run_attempt": 3, "path": ".github/workflows/ci.yml", "event": "push", "head_branch": "main", "head_sha": checkpointSHA, "status": "completed", "conclusion": "success", "repository": map[string]any{"full_name": "owner/project"}, "head_repository": map[string]any{"full_name": "owner/project"}}
	f.writeJSON(t, "run.json", run)
	f.writeJSON(t, "event.json", map[string]any{"action": "completed", "workflow_run": run, "repository": map[string]any{"full_name": "owner/project"}})
	f.inventory(t, map[string]any{"id": 800, "name": "Agent-Job-Queue-Installer-1", "expired": false, "digest": "sha256:" + strings.Repeat("c", 64), "workflow_run": map[string]any{"id": 123, "head_branch": "main", "head_sha": checkpointSHA}})
	f.writeJSON(t, "effective-jobs.json", map[string]any{"total_count": 1, "jobs": []map[string]any{
		{"id": 3, "run_id": 123, "name": "ci-required", "head_sha": checkpointSHA, "run_attempt": 3, "status": "completed", "conclusion": "success"}}})
	// A partial rerun can retain the successful installer artifact from attempt 1.
	f.writeJSON(t, "jobs.json", map[string]any{"total_count": 2, "jobs": []map[string]any{
		{"id": 1, "run_id": 123, "name": "Universal installer", "head_sha": checkpointSHA, "run_attempt": 1, "conclusion": "success"},
		{"id": 2, "run_id": 123, "name": "ci-required", "head_sha": checkpointSHA, "run_attempt": 1, "conclusion": "success"}}})
	return f
}

func (f checkpointFixture) changeJSON(t *testing.T, name string, change func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, name))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	change(value)
	f.writeJSON(t, name, value)
}

func TestDistributionSelectsOriginalArtifactAcrossCIPartialReruns(t *testing.T) {
	for _, event := range []string{"workflow_run", "workflow_dispatch"} {
		t.Run(event, func(t *testing.T) {
			f := newDistributionSourceFixture(t, event)
			if event == "workflow_dispatch" {
				f.changeJSON(t, "run.json", func(v map[string]any) { v["conclusion"] = "failure" })
			}
			output, err := f.run(t, nil, "select", "verified-source")
			if err != nil {
				t.Fatalf("select: %v: %s", err, output)
			}
			result, _ := os.ReadFile(filepath.Join(f.root, "output"))
			for _, want := range []string{"run_id=123\n", "run_number=45\n", "run_attempt=1\n", "sha=" + checkpointSHA + "\n", "artifact_id=800\n", "artifact_digest=sha256:" + strings.Repeat("c", 64) + "\n", "version=0.1.0\n", "tag=build-45-1111111\n", "automation_sha="} {
				if !strings.Contains(string(result), want) {
					t.Errorf("missing %q: %s", want, result)
				}
			}
			calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
			if !strings.Contains(string(calls), "/attempts/1/jobs") || !strings.Contains(string(calls), "/artifacts/800/zip") {
				t.Fatalf("wrong source selection: %s", calls)
			}
			verifier, _ := os.ReadFile(filepath.Join(f.root, "verifier-calls"))
			if !strings.Contains(string(verifier), "--commit "+checkpointSHA+" --build 45") {
				t.Fatalf("original bytes were not bound to build identity: %s", verifier)
			}
		})
	}
}

func TestDistributionRejectsAutomaticAndManualUntrustedSources(t *testing.T) {
	for _, variant := range []string{"manual feature", "manual nonnumeric", "manual leading zero", "automatic failure", "automatic fork", "automatic PR", "automatic branch", "automatic path", "automatic changed attempt", "source repository", "source SHA", "source path", "source event", "source unfinished", "source run", "source number", "missing artifact", "expired artifact", "wrong artifact SHA", "wrong artifact run", "malformed artifact name", "missing digest", "failed gate", "wrong gate SHA", "wrong gate attempt", "wrong gate run", "unfinished gate", "unvalidated installer"} {
		t.Run(variant, func(t *testing.T) {
			event := "workflow_dispatch"
			if strings.HasPrefix(variant, "automatic") {
				event = "workflow_run"
			}
			f := newDistributionSourceFixture(t, event)
			var extra []string
			switch variant {
			case "manual feature":
				extra = []string{"GITHUB_REF=refs/heads/feature"}
			case "manual nonnumeric":
				extra = []string{"INPUT_BUILD_RUN_ID=12;exit 0"}
			case "manual leading zero":
				extra = []string{"INPUT_BUILD_RUN_ID=0123"}
			case "automatic failure", "automatic fork", "automatic PR", "automatic branch", "automatic path", "automatic changed attempt":
				f.changeJSON(t, "event.json", func(v map[string]any) {
					r := v["workflow_run"].(map[string]any)
					switch variant {
					case "automatic failure":
						r["conclusion"] = "failure"
					case "automatic fork":
						r["head_repository"] = map[string]any{"full_name": "fork/project"}
					case "automatic PR":
						r["event"] = "pull_request"
					case "automatic branch":
						r["head_branch"] = "feature"
					case "automatic path":
						r["path"] = ".github/workflows/evil.yml"
					case "automatic changed attempt":
						r["run_attempt"] = 2
					}
				})
			case "source repository", "source SHA", "source path", "source event", "source unfinished", "source run", "source number":
				f.changeJSON(t, "run.json", func(v map[string]any) {
					switch variant {
					case "source repository":
						v["repository"] = map[string]any{"full_name": "fork/project"}
					case "source SHA":
						v["head_sha"] = "bad"
					case "source path":
						v["path"] = "evil"
					case "source event":
						v["event"] = "pull_request"
					case "source unfinished":
						v["status"] = "in_progress"
					case "source run":
						v["id"] = 124
					case "source number":
						v["run_number"] = 0
					}
				})
			case "missing artifact":
				f.inventory(t)
			case "expired artifact", "wrong artifact SHA", "wrong artifact run", "malformed artifact name", "missing digest":
				f.changeJSON(t, "page-1.json", func(v map[string]any) {
					a := v["artifacts"].([]any)[0].(map[string]any)
					switch variant {
					case "expired artifact":
						a["expired"] = true
					case "wrong artifact SHA":
						a["workflow_run"].(map[string]any)["head_sha"] = strings.Repeat("d", 40)
					case "wrong artifact run":
						a["workflow_run"].(map[string]any)["id"] = 456
					case "malformed artifact name":
						a["name"] = "Agent-Job-Queue-Installer-01"
					case "missing digest":
						delete(a, "digest")
					}
				})
			default:
				file := "effective-jobs.json"
				if variant == "unvalidated installer" {
					file = "jobs.json"
				}
				f.changeJSON(t, file, func(v map[string]any) {
					jobs := v["jobs"].([]any)
					j := jobs[0].(map[string]any)
					switch variant {
					case "failed gate":
						j["conclusion"] = "failure"
					case "wrong gate SHA":
						j["head_sha"] = strings.Repeat("e", 40)
					case "wrong gate attempt":
						j["run_attempt"] = 4
					case "wrong gate run":
						j["run_id"] = 456
					case "unfinished gate":
						j["status"] = "in_progress"
					case "unvalidated installer":
						jobs[0].(map[string]any)["conclusion"] = "failure"
					}
				})
			}
			output, err := f.run(t, extra, "select", "verified-source")
			if err == nil || !strings.Contains(output, "distribution:") {
				t.Fatalf("untrusted source accepted: %v: %s", err, output)
			}
			if _, err := os.Stat(filepath.Join(f.root, "verifier-calls")); !os.IsNotExist(err) {
				t.Fatal("untrusted source reached artifact processing")
			}
		})
	}
}

func TestDistributionDownloadRejectsSourceArtifactChanges(t *testing.T) {
	for _, field := range []string{"BUILD_RUN_ID", "BUILD_RUN_NUMBER", "BUILD_RUN_ATTEMPT", "BUILD_SHA", "BUILD_ARTIFACT_ID", "BUILD_ARTIFACT_DIGEST", "BUILD_VERSION", "BUILD_TAG"} {
		t.Run(field, func(t *testing.T) {
			f := newDistributionSourceFixture(t, "workflow_dispatch")
			extra := []string{"BUILD_RUN_ID=123", "BUILD_RUN_NUMBER=45", "BUILD_RUN_ATTEMPT=1", "BUILD_SHA=" + checkpointSHA, "BUILD_ARTIFACT_ID=800", "BUILD_ARTIFACT_DIGEST=sha256:" + strings.Repeat("c", 64), "BUILD_VERSION=0.1.0", "BUILD_TAG=build-45-1111111", field + "=999"}
			output, err := f.run(t, extra, "download", "verified-source")
			if err == nil {
				t.Fatalf("changed source identity accepted: %s", output)
			}
		})
	}
}

func TestDistributionSourcePaginatesArtifactInventory(t *testing.T) {
	f := newDistributionSourceFixture(t, "workflow_dispatch")
	raw, err := os.ReadFile(filepath.Join(f.root, "page-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var page map[string]any
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	page["total_count"] = 2
	f.writeJSON(t, "page-2.json", page)
	f.writeJSON(t, "page-1.json", map[string]any{"total_count": 2, "artifacts": []map[string]any{{"id": 12, "name": "report"}}})
	output, err := f.run(t, nil, "select", "verified-source")
	if err != nil {
		t.Fatalf("paginated source selection: %v: %s", err, output)
	}
	calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	if !strings.Contains(string(calls), "page=2") {
		t.Fatal(fmt.Sprintf("did not paginate: %s", calls))
	}
}

func TestDistributionSourceSeparatesInstallerAttemptFromEffectiveGate(t *testing.T) {
	for _, event := range []string{"workflow_run", "workflow_dispatch"} {
		t.Run(event, func(t *testing.T) {
			f := newDistributionSourceFixture(t, event)
			for _, name := range []string{"run.json", "event.json"} {
				f.changeJSON(t, name, func(v map[string]any) {
					if name == "event.json" {
						v = v["workflow_run"].(map[string]any)
					}
					v["run_attempt"] = 2
					if event == "workflow_dispatch" {
						v["conclusion"] = "failure"
					}
				})
			}
			f.changeJSON(t, "jobs.json", func(v map[string]any) {
				v["jobs"].([]any)[1].(map[string]any)["conclusion"] = "failure"
			})
			f.changeJSON(t, "effective-jobs.json", func(v map[string]any) {
				v["jobs"].([]any)[0].(map[string]any)["run_attempt"] = 2
			})
			output, err := f.run(t, nil, "select", "verified-source")
			if err != nil {
				t.Fatalf("valid partial CI retry rejected: %v: %s", err, output)
			}
			result, _ := os.ReadFile(filepath.Join(f.root, "output"))
			if !strings.Contains(string(result), "run_attempt=1\n") {
				t.Fatalf("lost original installer attempt: %s", result)
			}
			calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
			if !strings.Contains(string(calls), "/jobs?filter=latest") {
				t.Fatalf("effective validation was not checked: %s", calls)
			}
		})
	}
}

func TestDistributionSourceRequiresEffectiveGateForAutomaticAndManualRetries(t *testing.T) {
	for _, event := range []string{"workflow_run", "workflow_dispatch"} {
		for _, variant := range []string{"failed", "SHA", "attempt", "run", "missing", "duplicate"} {
			t.Run(event+"/"+variant, func(t *testing.T) {
				f := newDistributionSourceFixture(t, event)
				f.changeJSON(t, "effective-jobs.json", func(v map[string]any) {
					job := v["jobs"].([]any)[0].(map[string]any)
					switch variant {
					case "failed":
						job["conclusion"] = "failure"
					case "SHA":
						job["head_sha"] = strings.Repeat("e", 40)
					case "attempt":
						job["run_attempt"] = 4
					case "run":
						job["run_id"] = 456
					case "missing":
						v["jobs"] = []any{}
						v["total_count"] = 0
					case "duplicate":
						duplicate := map[string]any{}
						for k, value := range job {
							duplicate[k] = value
						}
						duplicate["id"] = 4
						v["jobs"] = append(v["jobs"].([]any), duplicate)
						v["total_count"] = 2
					}
				})
				output, err := f.run(t, nil, "select", "verified-source")
				if err == nil {
					t.Fatalf("invalid effective gate reached source processing: %s", output)
				}
				if _, err := os.Stat(filepath.Join(f.root, "verifier-calls")); !os.IsNotExist(err) {
					t.Fatal("invalid effective gate reached artifact verifier")
				}
			})
		}
	}
}
