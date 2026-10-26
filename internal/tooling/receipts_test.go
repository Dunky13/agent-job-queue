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
	return f
}

func (f resumeFixture) receipts(t *testing.T, extra []string, destination string) (string, error) {
	t.Helper()
	f.script, _ = filepath.Abs("../../scripts/macos-receipts.sh")
	return f.run(t, extra, f.state, destination, "payload", "900", strings.Repeat("d", 64))
}

func TestAppleReceiptsOnlyClassifyBoundPendingState(t *testing.T) {
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
	f := receiptFixture(t)
	if !strings.Contains(f.recorded(t), "Agent-Job-Queue-Payload-0.1.0-build-21-aaaaaaa-universal.zip") {
		t.Fatalf("submission omitted its original build identity: %s", f.recorded(t))
	}
}
