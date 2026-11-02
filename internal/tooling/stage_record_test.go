package tooling

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
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

// stageRecordToolWiring reports every job that runs a script reading stage records
// without first building queue-tool and handing that exact path to the step.
// Artifacts never carry the tool across jobs or architectures.
func stageRecordToolWiring(t *testing.T, workflow []byte) (consumers map[string]bool, problems []string) {
	t.Helper()
	var distribution struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string            `yaml:"name"`
				Run  string            `yaml:"run"`
				Uses string            `yaml:"uses"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &distribution); err != nil {
		t.Fatal(err)
	}
	const name = "QUEUE_NOTARY_TOOL"
	output := regexp.MustCompile(`go build .*-o "?([^" ]+)"? \./cmd/queue-tool`)
	export := regexp.MustCompile(`printf '` + name + `=%s\\n' "\$` + name + `" >> "\$GITHUB_ENV"`)
	normalize := func(path string) string { return strings.ReplaceAll(path, "$RUNNER_TEMP", "${{ runner.temp }}") }
	consumers = map[string]bool{}
	for job, definition := range distribution.Jobs {
		var built, exported string
		for _, step := range definition.Steps {
			if match := output.FindStringSubmatch(step.Run); match != nil {
				built = match[1]
				if built == "$"+name {
					built = step.Env[name]
				}
				built = normalize(built)
			}
			if export.MatchString(step.Run) {
				exported = normalize(step.Env[name])
			}
			if !strings.Contains(step.Run, "macos-resume.sh") && !strings.Contains(step.Run, "macos-receipts.sh") && !strings.Contains(step.Run, "distribution-publish.sh") && step.Uses != "./.github/actions/notarization-stage" {
				continue
			}
			consumers[job] = true
			delivered := exported
			if value, set := step.Env[name]; set {
				delivered = normalize(value)
			}
			if built == "" || delivered != built {
				problems = append(problems, fmt.Sprintf("%s: %q receives tool %q but the job built %q", job, step.Name, delivered, built))
			}
		}
	}
	return consumers, problems
}

func TestStageRecordToolIsBuiltBeforeEveryConsumer(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/distribution.yml")
	if err != nil {
		t.Fatal(err)
	}
	consumers, problems := stageRecordToolWiring(t, workflow)
	if len(problems) != 0 {
		t.Fatalf("broken tool wiring: %v", problems)
	}
	if !consumers["notarize"] || !consumers["publish"] || len(consumers) != 2 {
		t.Fatalf("unexpected stage record consumers: %v", consumers)
	}
	for mutation, change := range map[string][2]string{
		"notarize stops exporting":  {`          printf 'QUEUE_NOTARY_TOOL=%s\n' "$QUEUE_NOTARY_TOOL" >> "$GITHUB_ENV"` + "\n", ""},
		"notarize builds elsewhere": {`          go build -p 2 -trimpath -o "$QUEUE_NOTARY_TOOL" ./cmd/queue-tool`, `          go build -p 2 -trimpath -o "$RUNNER_TEMP/other" ./cmd/queue-tool`},
		"publish points elsewhere":  {"          GH_TOKEN: ${{ github.token }}\n          QUEUE_NOTARY_TOOL: ${{ runner.temp }}/queue-notary-tool\n", "          GH_TOKEN: ${{ github.token }}\n          QUEUE_NOTARY_TOOL: ${{ runner.temp }}/wrong-tool\n"},
		"publish loses the path":    {"          GH_TOKEN: ${{ github.token }}\n          QUEUE_NOTARY_TOOL: ${{ runner.temp }}/queue-notary-tool\n", "          GH_TOKEN: ${{ github.token }}\n"},
		"publish stops building":    {"      - name: Build the stage record validator from this automation commit\n        run: go build -p 2 -trimpath -o \"$RUNNER_TEMP/queue-notary-tool\" ./cmd/queue-tool\n", ""},
	} {
		t.Run(mutation, func(t *testing.T) {
			if !strings.Contains(string(workflow), change[0]) {
				t.Fatalf("mutation target moved: %s", change[0])
			}
			mutated := strings.Replace(string(workflow), change[0], change[1], 1)
			if _, problems := stageRecordToolWiring(t, []byte(mutated)); len(problems) == 0 {
				t.Fatal("broken wiring went unnoticed")
			}
		})
	}
	// The stage action inherits the exported path, so it must not replace it.
	action, err := os.ReadFile("../../.github/actions/notarization-stage/action.yml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(action), "QUEUE_NOTARY_TOOL") {
		t.Fatal("the notarization stage action overrides the exported tool path")
	}
	// The recovery observer has no Go toolchain, so its script cannot depend on the tool.
	recovery, err := os.ReadFile("../../.github/workflows/distribution-recovery.yml")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../scripts/distribution-recover.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), "QUEUE_NOTARY_TOOL") && !strings.Contains(string(recovery), "setup-go") {
		t.Fatal("distribution recovery can use the tool only in a job that builds it")
	}
}

func TestDistributionPublishValidatesStagesWithoutItsToken(t *testing.T) {
	t.Parallel()
	f := newPublishFixture(t)
	seen := filepath.Join(f.root, "tool-token")
	wrapper := filepath.Join(f.root, "tools", "queue-tool-wrapper")
	script := fmt.Sprintf("printf '%%s\\n' \"${GH_TOKEN-unset}\" >> %q\nexec %q \"$@\"\n", seen, stageRecordTool(t))
	testTool(t, filepath.Dir(wrapper), filepath.Base(wrapper), script)
	f.publish(t, "", true, "QUEUE_NOTARY_TOOL="+wrapper)
	data, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unset\nunset\n" {
		t.Fatalf("the record validator received the publication token: %q", data)
	}
}

// wrapUploadTool replaces the fake xcrun so a test can act around the provider
// call, while the real shell and the real record tool stay in place.
func wrapUploadTool(t *testing.T, f resumeFixture, beforeSubmit string) {
	t.Helper()
	// The fake dispatches on its own name, so it keeps that name in another directory.
	tools, fakes := filepath.Join(f.root, "tools"), filepath.Join(f.root, "fakes")
	if err := os.Mkdir(fakes, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(tools, "xcrun"), filepath.Join(fakes, "xcrun")); err != nil {
		t.Fatal(err)
	}
	testTool(t, tools, "xcrun", fmt.Sprintf("if [[ $1:$2 == notarytool:submit ]]; then %s\nfi\nexec %q \"$@\"\n", beforeSubmit, filepath.Join(fakes, "xcrun")))
}

// failAdvanceToSubmitted makes the record tool fail its submitted write, either
// instead of the write or after it reached the disk.
func failAdvanceToSubmitted(t *testing.T, f resumeFixture, script string) []string {
	t.Helper()
	wrapper := filepath.Join(f.root, "tools", "queue-tool-failing")
	testTool(t, filepath.Dir(wrapper), filepath.Base(wrapper), fmt.Sprintf("real=%q\nif [[ $* == *'record advance'*'--to submitted'* ]]; then %s\nfi\nexec \"$real\" \"$@\"\n", stageRecordTool(t), script))
	return []string{"QUEUE_NOTARY_TOOL=" + wrapper}
}

func TestMacOSResumeFailedWritesAroundTheProviderCallKeepCustody(t *testing.T) {
	const submission = "00000000-0000-0000-0000-000000000001"
	submits := func(t *testing.T, f resumeFixture) int {
		t.Helper()
		return strings.Count(f.recorded(t), "xcrun|notarytool|submit|")
	}
	t.Run("locked uploading write makes no provider call", func(t *testing.T) {
		f := newResumeFixture(t)
		f.step(t, "prepare", "app")
		stages, err := os.Open(filepath.Join(f.state, "stages"))
		if err != nil {
			t.Fatal(err)
		}
		defer stages.Close()
		if err := unix.Flock(int(stages.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		f.reject(t, nil, "record_locked", "submit", f.state, "app", "0.1.0")
		if phase := stagePhase(t, f, "app"); phase != "prepared" || submits(t, f) != 0 {
			t.Fatalf("locked write left phase %q after %d submits", phase, submits(t, f))
		}
		if err := unix.Flock(int(stages.Fd()), unix.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		f.step(t, "submit", "app")
		if phase := stagePhase(t, f, "app"); phase != "submitted" || submits(t, f) != 1 {
			t.Fatalf("released lock left phase %q after %d submits", phase, submits(t, f))
		}
	})
	t.Run("failed submitted write reports the id and refuses a rerun", func(t *testing.T) {
		f := newResumeFixture(t)
		f.step(t, "prepare", "app")
		// A read-only directory is a real write failure in the real record tool.
		stages := filepath.Join(f.state, "stages")
		wrapUploadTool(t, f, fmt.Sprintf("chmod 500 %q", stages))
		t.Cleanup(func() { _ = os.Chmod(stages, 0700) })
		output, err := f.run(t, nil, "submit", f.state, "app", "0.1.0")
		if err == nil || !strings.Contains(output, "record_persistence_failed") || !strings.Contains(output, "Apple submission "+submission+" could not be recorded") {
			t.Fatalf("failed write lost the only evidence of the ID: %v: %s", err, output)
		}
		if err := os.Chmod(stages, 0700); err != nil {
			t.Fatal(err)
		}
		if phase := stagePhase(t, f, "app"); phase != "uploading" {
			t.Fatalf("failed write left phase %q", phase)
		}
		f.reject(t, nil, "unknown outcome", "submit", f.state, "app", "0.1.0")
		if count := submits(t, f); count != 1 {
			t.Fatalf("rerun submitted again: %d", count)
		}
	})
	t.Run("provider text that cannot be an id is not echoed", func(t *testing.T) {
		f := newResumeFixture(t)
		f.step(t, "prepare", "app")
		wrapUploadTool(t, f, `printf '{"id":"private provider text"}\n'; exit 0`)
		output, err := f.run(t, nil, "submit", f.state, "app", "0.1.0")
		if err == nil || !strings.Contains(output, "malformed submission ID") || strings.Contains(output, "private provider text") {
			t.Fatalf("malformed ID handling: %v: %s", err, output)
		}
		if phase := stagePhase(t, f, "app"); phase != "uploading" {
			t.Fatalf("malformed ID left phase %q", phase)
		}
	})
	t.Run("failure after the rename stays submitted and continues", func(t *testing.T) {
		f := newResumeFixture(t)
		f.step(t, "prepare", "app")
		extra := failAdvanceToSubmitted(t, f, `"$real" "$@"; printf 'notarization: record_persistence_failed\n' >&2; exit 1`)
		output, err := f.run(t, extra, "submit", f.state, "app", "0.1.0")
		if err == nil || !strings.Contains(output, submission) {
			t.Fatalf("reported failure lost the ID: %v: %s", err, output)
		}
		if phase := stagePhase(t, f, "app"); phase != "submitted" {
			t.Fatalf("durable write left phase %q", phase)
		}
		if output := f.step(t, "submit", "app"); !strings.Contains(output, "submission already recorded") {
			t.Fatalf("rerun did not reuse the recorded submission: %s", output)
		}
		f.step(t, "finish", "app")
		if phase := stagePhase(t, f, "app"); phase != "finished" || submits(t, f) != 1 {
			t.Fatalf("continuation ended in %q after %d submits", phase, submits(t, f))
		}
	})
	t.Run("leftover temporary file is ignored", func(t *testing.T) {
		f := newResumeFixture(t)
		f.step(t, "prepare", "app")
		leftover := filepath.Join(f.state, "stages", ".notary-00000000000000000000000000000000")
		if err := os.WriteFile(leftover, []byte("partial write from a killed run"), 0600); err != nil {
			t.Fatal(err)
		}
		f.command(t, nil, "verify-state", f.state, "0.1.0")
		f.step(t, "submit", "app")
		f.step(t, "finish", "app")
		if phase := stagePhase(t, f, "app"); phase != "finished" || submits(t, f) != 1 {
			t.Fatalf("leftover changed the outcome: %q after %d submits", phase, submits(t, f))
		}
		if data, err := os.ReadFile(leftover); err != nil || string(data) != "partial write from a killed run" {
			t.Fatalf("leftover was consumed: %q %v", data, err)
		}
	})
}
