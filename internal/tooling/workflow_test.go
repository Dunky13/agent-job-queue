package tooling

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMacOSGoAndNativeValidationHaveIndependentBudgets(t *testing.T) {
	type validationStep struct {
		Run             string `yaml:"run"`
		If              string `yaml:"if"`
		ContinueOnError bool   `yaml:"continue-on-error"`
	}
	body, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			If              string   `yaml:"if"`
			RunsOn          string   `yaml:"runs-on"`
			Timeout         int      `yaml:"timeout-minutes"`
			ContinueOnError bool     `yaml:"continue-on-error"`
			Needs           []string `yaml:"needs"`
			Strategy        struct {
				FailFast *bool `yaml:"fail-fast"`
				Matrix   struct {
					OS []string `yaml:"os"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []validationStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	suites := map[string]string{
		"macos-tests":   "bash scripts/test-go-runtime.sh",
		"tooling-tests": "go test -p 2 -parallel 2 -race -v ./internal/tooling -timeout 12m",
		"native-tests":  "bash scripts/test-native.sh",
	}
	for name, command := range suites {
		job, exists := workflow.Jobs[name]
		if !exists {
			t.Fatalf("missing independent validation stage %s", name)
		}
		if job.Timeout != 20 || job.ContinueOnError || job.If != "" || job.RunsOn != "${{ matrix.os }}" || job.Strategy.FailFast == nil || *job.Strategy.FailFast || !reflect.DeepEqual(job.Strategy.Matrix.OS, []string{"macos-15", "macos-15-intel"}) {
			t.Fatalf("%s changed the existing budget or architecture requirements", name)
		}
		found := false
		for _, step := range job.Steps {
			if step.ContinueOnError || step.If != "" {
				t.Fatalf("%s may skip or ignore a validation failure", name)
			}
			found = found || strings.TrimSpace(step.Run) == command
			for other, otherCommand := range suites {
				if other != name && strings.Contains(step.Run, otherCommand) {
					t.Fatalf("%s combines unrelated validation phases", name)
				}
			}
		}
		if !found {
			t.Fatalf("%s omits its complete suite", name)
		}
	}
	gate := workflow.Jobs["ci-required"]
	if gate.If != "always()" || gate.ContinueOnError {
		t.Fatal("aggregate check must always require successful validation")
	}
	for name := range suites {
		if !slices.Contains(gate.Needs, name) {
			t.Fatalf("complete architecture matrix %s must remain required", name)
		}
	}
	if !slices.ContainsFunc(gate.Steps, func(step validationStep) bool {
		return !step.ContinueOnError && step.If == "" && strings.Contains(step.Run, `all(.[]; .result == "success")`)
	}) {
		t.Fatal("aggregate check no longer rejects incomplete validation")
	}
}

func TestWorkflowPreservesDownloadedArtifacts(t *testing.T) {
	// Checkout cleans untracked and ignored workspace files by default. Main
	// publication previously downloaded dist first, then deleted it at checkout.
	body, err := os.ReadFile("../../.github/workflows/distribution.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string `yaml:"uses"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.Jobs) == 0 {
		t.Fatal("workflow contains no jobs")
	}
	for job, definition := range workflow.Jobs {
		downloaded := false
		for _, step := range definition.Steps {
			if downloaded && strings.HasPrefix(step.Uses, "actions/checkout@") {
				t.Errorf("%s: checkout can delete downloaded artifacts; check out before downloading", job)
			}
			downloaded = downloaded || strings.HasPrefix(step.Uses, "actions/download-artifact@")
		}
	}
}

func TestNotarizationPersistsReceiptsBeforeWaiting(t *testing.T) {
	// A successful Apple upload must reach durable storage before any wait.
	// Otherwise a timeout or runner loss could turn a retry into a duplicate.
	body, err := os.ReadFile("../../.github/actions/notarization-stage/action.yml")
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Runs struct {
			Steps []struct {
				ID              string            `yaml:"id"`
				Run             string            `yaml:"run"`
				Uses            string            `yaml:"uses"`
				If              string            `yaml:"if"`
				ContinueOnError bool              `yaml:"continue-on-error"`
				With            map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(body, &action); err != nil {
		t.Fatal(err)
	}
	var operations []string
	for _, step := range action.Runs.Steps {
		if step.ContinueOnError {
			t.Fatal("notarization must stop after failed signing, persistence or Apple acceptance")
		}
		if strings.Contains(step.Run, "macos-resume.sh prepare ") && step.ID != "prepare" {
			t.Fatal("preparation must expose its outcome for failure checkpointing")
		}
		if step.ID == "finished-snapshot" && step.If != "always() && steps.prepare.outcome != 'skipped'" {
			t.Fatal("the final checkpoint must run after every attempted preparation, including failure before submission")
		}
		for _, operation := range []string{"prepare", "submit", "finish"} {
			if strings.Contains(step.Run, "macos-resume.sh "+operation+" ") {
				if step.If != "" {
					t.Fatalf("%s must require all preceding persistence steps to succeed", operation)
				}
				operations = append(operations, operation)
			}
		}
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			if step.With["overwrite"] == "true" || step.With["retention-days"] != "90" || step.With["if-no-files-found"] != "error" {
				t.Fatalf("checkpoint upload must be immutable, required and retained: %+v", step.With)
			}
			if strings.HasPrefix(step.With["name"], "Apple-notarization-") {
				operations = append(operations, "persist")
			}
			if strings.Contains(step.With["name"], "finished-slot") && !strings.Contains(step.If, "always()") {
				t.Fatal("pending or rejected state must survive a failed wait")
			}
		}
	}
	if expected := []string{"prepare", "persist", "submit", "persist", "finish", "persist"}; !reflect.DeepEqual(operations, expected) {
		t.Fatalf("unsafe notarization ordering: %v, want %v", operations, expected)
	}
}

func TestNotarizationUsesTrustedAttemptScopedArtifacts(t *testing.T) {
	body, err := os.ReadFile("../../.github/workflows/distribution.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			If          string            `yaml:"if"`
			Environment string            `yaml:"environment"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Run  string            `yaml:"run"`
				Uses string            `yaml:"uses"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	signing := workflow.Jobs["notarize"]
	if signing.If != "" || signing.Environment != "apple-signing" || signing.Permissions["actions"] != "read" {
		t.Fatal("resumption must preserve the protected validated-source dependency and read-only artifact access")
	}
	var stages, slots []string
	for _, step := range signing.Steps {
		if step.Uses == "./.github/actions/notarization-stage" {
			stages = append(stages, step.With["stage"])
			slots = append(slots, step.With["prepared-slot"], step.With["submitted-slot"], step.With["finished-slot"])
		}
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && (!strings.Contains(step.With["name"], "github.run_attempt") || step.With["overwrite"] == "true") {
			t.Fatal("reruns must preserve previous final artifacts with unique attempt-scoped names")
		}
	}
	if !reflect.DeepEqual(stages, []string{"payload", "installer", "dmg"}) || !reflect.DeepEqual(slots, []string{"01", "02", "03", "04", "05", "06", "07", "08", "09"}) {
		t.Fatalf("incorrect checkpoint ordering: stages=%v slots=%v", stages, slots)
	}
	for _, job := range []string{"notarized-tests", "publish", "homebrew-sync"} {
		found := false
		for _, step := range workflow.Jobs[job].Steps {
			if strings.HasPrefix(step.Uses, "actions/download-artifact@") {
				found = step.With["name"] == "${{ needs.notarize.outputs.artifact_name }}"
			}
		}
		if !found {
			t.Errorf("%s must consume the exact successful signing attempt's artifact", job)
		}
	}
}

func TestNotarizationPersistsInitialStateBeforeCredentials(t *testing.T) {
	body, err := os.ReadFile("../../.github/workflows/distribution.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run             string            `yaml:"run"`
				Uses            string            `yaml:"uses"`
				If              string            `yaml:"if"`
				ContinueOnError bool              `yaml:"continue-on-error"`
				With            map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	verified, snapshotted, persisted, stageSeen := false, false, false, false
	for _, step := range workflow.Jobs["notarize"].Steps {
		if strings.Contains(step.Run, "macos-resume.sh verify-state") {
			verified = true
		}
		if strings.Contains(step.Run, "macos-checkpoint.sh snapshot") {
			if !verified {
				t.Fatal("initial state must be verified before snapshotting")
			}
			snapshotted = true
		}
		if strings.HasSuffix(step.With["name"], "-s00") {
			if !snapshotted || !strings.HasPrefix(step.Uses, "actions/upload-artifact@") || step.With["if-no-files-found"] != "error" || step.With["retention-days"] != "90" || step.With["overwrite"] == "true" || step.If != "" || step.ContinueOnError {
				t.Fatal("a required immutable initial checkpoint must precede credential setup")
			}
			persisted = true
		}
		if step.Uses == "./.github/actions/notarization-stage" {
			if !persisted {
				t.Fatal("signing can fail before any recoverable state is retained")
			}
			stageSeen = true
		}
	}
	if !stageSeen || !persisted {
		t.Fatal("workflow lacks initial checkpoint or signing stages")
	}
}

func TestDistributionWorkflowTrustAndTopology(t *testing.T) {
	type step struct {
		Uses, Run string
		With      map[string]string
	}
	type job struct {
		Needs       []string
		Environment string
		Permissions map[string]string
		Steps       []step
		Env         map[string]string
	}
	type flow struct {
		On          map[string]any
		Permissions map[string]string
		Concurrency struct {
			Group  string
			Cancel any `yaml:"cancel-in-progress"`
		}
		Jobs map[string]job
	}
	read := func(name string) flow {
		t.Helper()
		data, err := os.ReadFile("../../.github/workflows/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var f flow
		if err := yaml.Unmarshal(data, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	ci, dist := read("ci.yml"), read("distribution.yml")
	for _, name := range []string{"source", "notarize", "notarized-tests", "publish", "homebrew-sync"} {
		if _, ok := ci.Jobs[name]; ok {
			t.Errorf("CI still performs distribution: %s", name)
		}
	}
	if _, ok := ci.Jobs["installer"]; !ok {
		t.Fatal("CI lost installer validation")
	}
	if dist.Permissions["contents"] != "read" || dist.Permissions["actions"] != "read" || dist.Concurrency.Cancel != false || !strings.Contains(dist.Concurrency.Group, "inputs.build_run_id || github.event.workflow_run.id") {
		t.Fatal("distribution lost read-only default or source-build serialization")
	}
	if !strings.HasPrefix(ci.Concurrency.Group, "ci-") || !strings.Contains(ci.Concurrency.Group, "github.run_id") || dist.Concurrency.Group != "ci-${{ inputs.build_run_id || github.event.workflow_run.id }}" {
		t.Fatal("distribution must share the original main CI run's concurrency lock so legacy signing cannot overlap recovery")
	}
	for name, want := range map[string][]string{"notarize": {"source"}, "notarized-tests": {"source", "notarize"}, "publish": {"source", "notarize", "notarized-tests"}, "homebrew-sync": {"source", "publish", "notarize"}} {
		if !reflect.DeepEqual(dist.Jobs[name].Needs, want) {
			t.Errorf("%s gate topology: %v", name, dist.Jobs[name].Needs)
		}
	}
	for name, j := range dist.Jobs {
		if name == "notarize" {
			if j.Environment != "apple-signing" {
				t.Error("signing environment missing")
			}
		} else if j.Environment != "" {
			t.Errorf("%s unnecessarily receives Apple environment", name)
		}
		if name != "publish" && j.Permissions["contents"] == "write" {
			t.Errorf("%s can publish before acceptance", name)
		}
		for _, s := range j.Steps {
			if strings.Contains(s.Run, "build-installer") || strings.Contains(s.Run, "cat VERSION") {
				t.Errorf("%s rebuilds source or uses automation VERSION", name)
			}
			if strings.HasPrefix(s.Uses, "actions/checkout@") {
				ref := "${{ needs.source.outputs.automation_sha }}"
				if name == "source" {
					ref = "${{ github.sha }}"
				}
				if s.With["ref"] != ref || s.With["persist-credentials"] != "false" {
					t.Errorf("%s has unpinned or credentialed checkout: %v", name, s.With)
				}
			}
			if strings.Contains(s.Run, "GITHUB_ENV") && strings.Contains(s.Run, "GITHUB_RUN_ID=") {
				t.Fatal("distribution overwrites built-in execution identity")
			}
		}
	}
	if dist.Jobs["publish"].Permissions["contents"] != "write" {
		t.Fatal("publication permissions missing")
	}
	body, err := os.ReadFile("../../.github/workflows/distribution.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, guard := range []string{"types: [completed]", "branches: [main]", "github.ref == 'refs/heads/main'", "github.event.workflow_run.conclusion == 'success'", "github.event.workflow_run.event == 'push'", "github.event.workflow_run.path == '.github/workflows/ci.yml'", "github.event.workflow_run.repository.full_name == github.repository", "github.event.workflow_run.head_repository.full_name == github.repository", "required: true", "--commit \"$BUILD_SHA\"", "scripts/distribution-publish.sh dist"} {
		if !strings.Contains(string(body), guard) {
			t.Errorf("missing source/publication guard %s", guard)
		}
	}
	action, err := os.ReadFile("../../.github/actions/notarization-stage/action.yml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(action), "cat VERSION") || !strings.Contains(string(action), "$BUILD_VERSION") || !strings.Contains(string(action), "env.BUILD_RUN_ID") {
		t.Fatal("composite action lost explicit source identity")
	}
}
