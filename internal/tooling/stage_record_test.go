package tooling

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

var sharedStageRecordTool struct {
	once       sync.Once
	dir, path  string
	buildError error
}

// stageRecordTool builds the production queue-tool once per test binary, so
// every shard drives the real record rules through the real shell scripts.
func stageRecordTool(t *testing.T) string {
	t.Helper()
	shared := &sharedStageRecordTool
	shared.once.Do(func() {
		if shared.dir, shared.buildError = os.MkdirTemp("", "stage-record-tool-"); shared.buildError != nil {
			return
		}
		shared.path = filepath.Join(shared.dir, "queue-tool")
		build := exec.Command("go", "build", "-p", "2", "-o", shared.path, "../../cmd/queue-tool")
		build.Env = signingTestEnvironment()
		if output, err := build.CombinedOutput(); err != nil {
			shared.buildError = fmt.Errorf("%w: %s", err, output)
		}
	})
	if shared.buildError != nil {
		t.Fatalf("build queue-tool: %v", shared.buildError)
	}
	return shared.path
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedStageRecordTool.dir != "" {
		_ = os.RemoveAll(sharedStageRecordTool.dir)
	}
	os.Exit(code)
}

func TestMacOSResumeFailedPreparationCheckpointStillResumes(t *testing.T) {
	f := receiptFixture(t)
	if err := os.Remove(filepath.Join(f.state, "stages/app.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.state, "archives/app.zip")); err != nil {
		t.Fatal(err)
	}
	f.reject(t, []string{"SIGNING_TEST_FAILURE=app-sign-heavy"}, "operation=sign-app-agent-heavy phase=failure", "prepare", f.state, "app", "0.1.0")
	f.command(t, nil, "verify-state", f.state, "0.1.0")

	// The checkpoint of this failed attempt has no record, and its receipt says so.
	out := filepath.Join(f.root, "failed-preparation-receipts")
	if output, err := f.receipts(t, nil, out); err != nil {
		t.Fatalf("receipts without a stage record: %v: %s", err, output)
	}
	body, err := os.ReadFile(filepath.Join(out, "recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Phase      string `json:"phase"`
		Reason     string `json:"reason"`
		Submission string `json:"submission_id"`
		Archive    string `json:"archive_sha256"`
	}
	if err := json.Unmarshal(body, &receipt); err != nil || receipt.Phase != "prepared" || receipt.Reason != "none" || receipt.Submission != "" || receipt.Archive != "" {
		t.Fatalf("failed preparation receipt: %s (%v)", body, err)
	}

	f.step(t, "prepare", "app")
	f.step(t, "submit", "app")
	f.step(t, "finish", "app")
	if phase := stagePhase(t, f, "app"); phase != "finished" {
		t.Fatalf("resumed stage ended in %q", phase)
	}
}

func stagePhase(t *testing.T, f resumeFixture, stage string) string {
	t.Helper()
	output, err := exec.Command(stageRecordTool(t), "notarize", "record", "get", "--record", filepath.Join(f.state, "stages", stage+".json"), "--field", "phase").CombinedOutput()
	if err != nil {
		t.Fatalf("read phase: %v: %s", err, output)
	}
	return strings.TrimSpace(string(output))
}

// Exact text of records that earlier runs wrote with jq, which is what a
// restored checkpoint holds. A rewrite in Go must not be needed to read them.
func earlierRunStageRecord(phase, hash, id, runKey string) string {
	return fmt.Sprintf("{\n  \"stage\": \"app\",\n  \"phase\": %q,\n%s  \"prepared_attempt\": \"1\",\n  \"archive_sha256\": %q,\n  \"submission_id\": %q,\n  \"completed_sha256\": \"\"\n}\n", phase, runKey, hash, id)
}

func TestMacOSResumeContinuesRecordsWrittenByEarlierRuns(t *testing.T) {
	const withRun, legacy = "  \"prepared_run_id\": \"1234\",\n", ""
	const submitted = "00000000-0000-0000-0000-000000000001"
	for _, test := range []struct {
		name, phase, id, run string
		next                 []string
	}{
		{"prepared with run", "prepared", "", withRun, []string{"submit", "finish"}},
		{"legacy prepared in source run", "prepared", "", legacy, []string{"submit", "finish"}},
		{"submitted with run", "submitted", submitted, withRun, []string{"finish"}},
		{"legacy submitted", "submitted", submitted, legacy, []string{"finish"}},
		{"legacy accepted", "accepted", submitted, legacy, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newResumeFixture(t)
			f.step(t, "prepare", "app")
			path := filepath.Join(f.state, "stages/app.json")
			var current struct {
				Hash string `json:"archive_sha256"`
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &current); err != nil {
				t.Fatal(err)
			}
			// Provider and acceptance evidence comes from real steps; only the record text is restored.
			if test.phase != "prepared" {
				f.step(t, "submit", "app")
			}
			if test.phase == "accepted" {
				f.reject(t, []string{"SIGNING_TEST_FAILURE=app-staple"}, "", "finish", f.state, "app", "0.1.0")
				if phase := stagePhase(t, f, "app"); phase != "accepted" {
					t.Fatalf("staple failure left the stage in %q", phase)
				}
				test.next = []string{"finish"}
			}
			if err := os.WriteFile(path, []byte(earlierRunStageRecord(test.phase, current.Hash, test.id, test.run)), 0600); err != nil {
				t.Fatal(err)
			}
			f.command(t, nil, "verify-state", f.state, "0.1.0")
			for _, operation := range test.next {
				f.step(t, operation, "app")
			}
			if phase := stagePhase(t, f, "app"); phase != "finished" {
				t.Fatalf("restored record ended in %q", phase)
			}
			var finished map[string]any
			data, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &finished); err != nil {
				t.Fatal(err)
			}
			if _, added := finished["prepared_run_id"]; added != (test.run == withRun) {
				t.Fatalf("writer changed the optional run key: %s", data)
			}
		})
	}
}

// Every job that runs a script reading stage records must first build
// queue-tool from its own checkout. Artifacts never carry the tool across jobs
// or architectures. The recovery observer has no Go toolchain, so its script
// must not depend on the tool.
func TestStageRecordToolIsBuiltBeforeEveryConsumer(t *testing.T) {
	type step struct {
		Name string            `yaml:"name"`
		Run  string            `yaml:"run"`
		Uses string            `yaml:"uses"`
		Env  map[string]string `yaml:"env"`
	}
	var distribution struct {
		Jobs map[string]struct {
			Steps []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	body, err := os.ReadFile("../../.github/workflows/distribution.yml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(body, &distribution); err != nil {
		t.Fatal(err)
	}
	consumers := map[string]bool{}
	for name, job := range distribution.Jobs {
		built := false
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "go build") && strings.Contains(step.Run, "./cmd/queue-tool") {
				built = true
			}
			if strings.Contains(step.Run, "macos-resume.sh") || strings.Contains(step.Run, "macos-receipts.sh") || strings.Contains(step.Run, "distribution-publish.sh") || step.Uses == "./.github/actions/notarization-stage" {
				consumers[name] = true
				if !built {
					t.Errorf("%s: %q runs before queue-tool is built in this job", name, step.Name)
				}
			}
			if strings.Contains(step.Run, "distribution-publish.sh") && step.Env["QUEUE_NOTARY_TOOL"] == "" {
				t.Errorf("%s: publication does not receive the tool it built", name)
			}
		}
	}
	if !consumers["notarize"] || !consumers["publish"] || len(consumers) != 2 {
		t.Fatalf("unexpected stage record consumers: %v", consumers)
	}
	recover, err := os.ReadFile("../../.github/workflows/distribution-recovery.yml")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../scripts/distribution-recover.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), "QUEUE_NOTARY_TOOL") && !strings.Contains(string(recover), "setup-go") {
		t.Fatal("distribution recovery can use the tool only in a job that builds it")
	}
}
