package tooling

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func receiptFixture(t *testing.T) resumeFixture {
	t.Helper()
	f := newRESTResumeFixture(t)
	f.environment = append(f.environment, "GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_REF=refs/heads/main",
		"BUILD_SHA="+strings.Repeat("a", 40), "BUILD_RUN_ID=1234", "BUILD_RUN_NUMBER=21", "BUILD_RUN_ATTEMPT=1",
		"BUILD_ARTIFACT_ID=800", "BUILD_ARTIFACT_DIGEST=sha256:"+strings.Repeat("c", 64), "BUILD_VERSION=0.1.0")
	if err := os.RemoveAll(f.state); err != nil {
		t.Fatal(err)
	}
	f.command(t, nil, "init", f.installer, f.state, "0.1.0")
	f.step(t, "prepare", "payload")
	f.step(t, "submit", "payload")
	f.signingOutcome(t, "payload", "success", "1")
	return f
}

func (f *resumeFixture) signingOutcome(t *testing.T, stage, cleanup, attempt string) {
	t.Helper()
	path := filepath.Join(f.root, "signing-outcome-"+stage+".json")
	data, err := json.Marshal(map[string]any{"cleanup": cleanup, "operation": stage + "-finish", "execution_id": "1234", "execution_attempt": attempt, "command_status": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.environment = append(f.environment, "APPLE_SIGNING_RESULT_FILE="+path)
}

func TestAppleReceiptsRequireCurrentInvocationAndSuccessfulCleanup(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"inherited attempt", "cleanup failed", "cleanup missing", "stage not entered"} {
		t.Run(variant, func(t *testing.T) {
			f := receiptFixture(t)
			f.reject(t, []string{"SIGNING_TEST_FAILURE=payload-timeout"}, "", "finish", f.state, "payload", "0.1.0")
			switch variant {
			case "inherited attempt":
				f.environment = append(f.environment, "GITHUB_RUN_ATTEMPT=2")
				f.signingOutcome(t, "payload", "success", "2")
			case "cleanup failed":
				f.signingOutcome(t, "payload", "failed", "1")
			case "cleanup missing":
				f.environment = append(f.environment, "APPLE_SIGNING_RESULT_FILE=")
			case "stage not entered":
				f.step(t, "begin", "payload")
			}
			out := filepath.Join(f.root, "public-receipts")
			if output, err := f.receipts(t, nil, out); err != nil {
				t.Fatalf("public receipts: %v: %s", err, output)
			}
			body, _ := os.ReadFile(filepath.Join(out, "recovery.json"))
			var record struct{ Reason string }
			if err := json.Unmarshal(body, &record); err != nil || record.Reason != "none" {
				t.Fatalf("stale or failed cleanup authorized continuation: %s (%v)", body, err)
			}
		})
	}
}

func TestAppleExportTicketFailureRetainsFinishedCandidateForContinuation(t *testing.T) {
	t.Parallel()
	f := receiptFixture(t)
	f.step(t, "finish", "payload")
	for _, stage := range []string{"installer", "dmg"} {
		for _, op := range []string{"prepare", "submit", "finish"} {
			f.step(t, op, stage)
		}
	}
	f.signingOutcome(t, "dmg", "success", "1")
	before, err := os.ReadFile(filepath.Join(f.state, "completed/dmg.dmg"))
	if err != nil {
		t.Fatal(err)
	}
	f.injectTicketFailures(t, "validate", "persistent")
	f.reject(t, nil, "exhausted 6 attempts", "export", f.state, f.destination, "0.1.0")
	copy := f
	copy.script, _ = filepath.Abs("../../scripts/macos-receipts.sh")
	out := filepath.Join(f.root, "export-receipts")
	if output, err := copy.run(t, nil, f.state, out, "dmg", "910", strings.Repeat("d", 64)); err != nil {
		t.Fatalf("export receipts: %v: %s", err, output)
	}
	body, _ := os.ReadFile(filepath.Join(out, "recovery.json"))
	var record struct{ Reason, Phase string }
	if err := json.Unmarshal(body, &record); err != nil || record.Reason != "ticket-connection-loss" || record.Phase != "finished" {
		t.Fatalf("export failure lost continuation authority: %s (%v)", body, err)
	}
	calls := f.recorded(t)
	f.command(t, []string{"SIGNING_TEST_TICKET_OUTCOME=valid", "GITHUB_RUN_ATTEMPT=2"}, "export", f.state, f.destination, "0.1.0")
	after, _ := os.ReadFile(filepath.Join(f.state, "completed/dmg.dmg"))
	if string(before) != string(after) || strings.Contains(strings.TrimPrefix(f.recorded(t), calls), "rest-create|") {
		t.Fatal("export continuation changed completed bytes or created an Apple submission")
	}
}

func (f resumeFixture) receipts(t *testing.T, extra []string, destination string) (string, error) {
	t.Helper()
	f.script, _ = filepath.Abs("../../scripts/macos-receipts.sh")
	return f.run(t, extra, f.state, destination, "payload", "900", strings.Repeat("d", 64))
}

func TestAppleReceiptsOnlyClassifyBoundPendingState(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"payload-timeout", "payload-rejected", "waitid", "logid", "loghash", "logstatus"} {
		t.Run(failure, func(t *testing.T) {
			f := receiptFixture(t)
			f.reject(t, []string{"SIGNING_TEST_FAILURE=" + failure}, "", "finish", f.state, "payload", "0.1.0")
			if err := os.WriteFile(filepath.Join(f.state, "private.p8"), []byte("never export private state"), 0600); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(f.root, "receipts")
			output, err := f.receipts(t, nil, out)
			if err != nil {
				t.Fatalf("receipts: %v: %s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(out, "recovery.json"))
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				Reason, Phase string
				SubmissionID  string `json:"submission_id"`
				CheckpointID  string `json:"checkpoint_id"`
			}
			if err := json.Unmarshal(body, &record); err != nil {
				t.Fatal(err)
			}
			want := "none"
			if failure == "payload-timeout" {
				want = "pending"
			}
			if record.Reason != want || record.Phase != "submitted" || record.CheckpointID != "900" || record.SubmissionID != "00000000-0000-0000-0000-000000000001" {
				t.Fatalf("incorrect continuation authority: %s", body)
			}
			entries, _ := os.ReadDir(out)
			for _, entry := range entries {
				data, _ := os.ReadFile(filepath.Join(out, entry.Name()))
				if strings.Contains(string(data), "must never persist") || strings.Contains(string(data), "never export") || strings.Contains(entry.Name(), "private") {
					t.Fatalf("private material leaked in %s", entry.Name())
				}
			}
		})
	}
}

func TestAppleReceiptsRejectUnboundSourceAndUnsafeFiles(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"different build", "different artifact", "different team", "unsafe status", "wrong filename"} {
		t.Run(variant, func(t *testing.T) {
			f := receiptFixture(t)
			var extra []string
			switch variant {
			case "different build":
				extra = []string{"BUILD_RUN_ID=54321"}
			case "different artifact":
				extra = []string{"BUILD_ARTIFACT_ID=801"}
			case "different team":
				extra = []string{"APPLE_TEAM_ID=AAAAAAAAAA"}
			case "unsafe status":
				if err := os.Symlink(filepath.Join(f.state, "metadata.json"), filepath.Join(f.state, "receipts/payload.status.json")); err != nil {
					t.Fatal(err)
				}
			case "wrong filename":
				f.step(t, "finish", "payload")
				f.mutate(t, "receipts/payload.log.json", func(v map[string]any) { v["archiveFilename"] = "Other-Product.zip" })
			}
			if output, err := f.receipts(t, extra, filepath.Join(f.root, "receipts")); err == nil {
				t.Fatalf("unbound evidence was exported: %s", output)
			}
		})
	}
}

func TestAppleSubmissionNameIncludesOriginalBuild(t *testing.T) {
	t.Parallel()
	f := receiptFixture(t)
	if !strings.Contains(f.recorded(t), "Agent-Job-Queue-Payload-0.1.0-build-21-aaaaaaa-universal.zip") {
		t.Fatalf("submission omitted its original build identity: %s", f.recorded(t))
	}
}

func publicationFixture(t *testing.T) resumeFixture {
	t.Helper()
	f := receiptFixture(t)
	f.step(t, "finish", "payload")
	for _, stage := range []string{"installer", "dmg"} {
		for _, operation := range []string{"prepare", "submit", "finish"} {
			f.step(t, operation, stage)
		}
	}
	path := filepath.Join(f.root, "toolchain.json")
	data, _ := json.Marshal(map[string]any{
		"schema": 1, "source_sha": strings.Repeat("a", 40), "automation_sha": strings.Repeat("a", 40),
		"xcode": "16.4", "xcode_build": "16F6", "macos_sdk": "15.5", "swift": "Apple Swift fixture",
		"go": "go version fixture", "notarytool": "1.1.0 fixture", "os": "15.7.9", "host_architecture": "arm64",
		"runner_image": "macos15", "runner_image_version": "fixture",
	})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.environment = append(f.environment, "APPLE_TOOLCHAIN_RECEIPT="+path)
	f.command(t, nil, "evidence", f.state, "0.1.0")
	return f
}

func TestPublicationEvidenceRetainsExactBytesAcrossWholeWorkflowRecovery(t *testing.T) {
	t.Parallel()
	f := publicationFixture(t)
	path := filepath.Join(f.state, "publication.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(f.root, "first-export")
	f.command(t, nil, "export", f.state, first, "0.1.0")
	// A later execution has a different runner/toolchain receipt, execution ID,
	// attempt and automation revision. The candidate's public audit asset stays
	// bound to the first finalization instead of becoming different release bytes.
	if err := os.Remove(filepath.Join(f.root, "toolchain.json")); err != nil {
		t.Fatal(err)
	}
	later := []string{"GITHUB_RUN_ID=9999", "GITHUB_RUN_NUMBER=100", "GITHUB_RUN_ATTEMPT=2", "GITHUB_SHA=" + strings.Repeat("b", 40)}
	calls := f.recorded(t)
	f.command(t, later, "evidence", f.state, "0.1.0")
	second := filepath.Join(f.root, "second-export")
	f.command(t, later, "export", f.state, second, "0.1.0")
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("whole-workflow recovery rewrote the immutable public evidence asset")
	}
	for _, suffix := range []string{"zip", "zip.sha256", "dmg", "dmg.sha256"} {
		name := "Agent-Job-Queue-Installer-0.1.0-universal." + suffix
		left, err := os.ReadFile(filepath.Join(first, name))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(second, name))
		if err != nil || string(left) != string(right) {
			t.Fatalf("whole-workflow recovery changed %s: %v", name, err)
		}
	}
	if strings.Contains(strings.TrimPrefix(f.recorded(t), calls), "rest-create|") {
		t.Fatal("publication recovery created another Apple submission")
	}
}

func TestPublicationEvidenceRejectsUnboundRetainedContents(t *testing.T) {
	t.Parallel()
	f := publicationFixture(t)
	path := filepath.Join(f.state, "publication.json")
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"metadata", "stage", "log", "toolchain", "unknown", "toolchain missing", "toolchain unknown", "toolchain empty", "toolchain type", "toolchain architecture", "JSON stream", "duplicate top key", "duplicate nested key"} {
		t.Run(field, func(t *testing.T) {
			// Each refusal has a fresh copy of the same completed candidate;
			// failed verification must never alter its other immutable state.
			if err := os.WriteFile(path, valid, 0600); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(field, "duplicate") {
				changed := "{\"toolchain\":{\"private\":\"must not publish\"}," + string(valid[1:])
				if field == "duplicate nested key" {
					changed = strings.Replace(string(valid), `"toolchain": {`, `"toolchain": {"go":{"private":"must not publish"},`, 1)
				}
				if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
					t.Fatal(err)
				}
				f.reject(t, nil, "Retained publication evidence", "evidence", f.state, "0.1.0")
				return
			}
			if field == "JSON stream" {
				if err := os.WriteFile(path, append([]byte("{\"private\":\"must not publish\"}\n"), valid...), 0600); err != nil {
					t.Fatal(err)
				}
				f.reject(t, nil, "Retained publication evidence", "evidence", f.state, "0.1.0")
				return
			}
			f.mutate(t, "publication.json", func(v map[string]any) {
				switch field {
				case "metadata":
					v["metadata"].(map[string]any)["source_sha"] = strings.Repeat("c", 40)
				case "stage":
					v["stages"].([]any)[0].(map[string]any)["submission_id"] = "00000000-0000-0000-0000-000000000009"
				case "log":
					v["logs"].([]any)[0].(map[string]any)["status"] = "Rejected"
				case "toolchain":
					v["toolchain"].(map[string]any)["automation_sha"] = strings.Repeat("c", 40)
				case "unknown":
					v["private"] = "must not become a public asset"
				case "toolchain missing":
					delete(v["toolchain"].(map[string]any), "swift")
				case "toolchain unknown":
					v["toolchain"].(map[string]any)["private"] = "must not publish"
				case "toolchain empty":
					v["toolchain"].(map[string]any)["macos_sdk"] = ""
				case "toolchain type":
					v["toolchain"].(map[string]any)["go"] = 1
				case "toolchain architecture":
					v["toolchain"].(map[string]any)["host_architecture"] = "other"
				}
			})
			f.reject(t, nil, "Retained publication evidence", "evidence", f.state, "0.1.0")
		})
	}
}
