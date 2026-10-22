package tooling

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkflowPreservesDownloadedArtifacts(t *testing.T) {
	// Checkout cleans untracked and ignored workspace files by default. Main
	// publication previously downloaded dist first, then deleted it at checkout.
	body, err := os.ReadFile("../../.github/workflows/ci.yml")
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
		for _, operation := range []string{"prepare", "submit", "finish"} {
			if strings.Contains(step.Run, "macos-resume.sh "+operation+" ") {
				if step.If != "" {
					t.Fatalf("%s must require all preceding persistence steps to succeed", operation)
				}
				operations = append(operations, operation)
			}
		}
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			if step.With["overwrite"] == "true" || step.With["retention-days"] != "30" || step.With["if-no-files-found"] != "error" {
				t.Fatalf("checkpoint upload must be immutable, required and retained: %+v", step.With)
			}
			operations = append(operations, "persist")
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
	body, err := os.ReadFile("../../.github/workflows/ci.yml")
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
	if signing.If != "github.event_name == 'push' && github.ref == 'refs/heads/main'" || signing.Environment != "apple-signing" || signing.Permissions["actions"] != "read" {
		t.Fatal("resumption must preserve the protected main push boundary and read-only artifact access")
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
	body, err := os.ReadFile("../../.github/workflows/ci.yml")
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
			if !snapshotted || !strings.HasPrefix(step.Uses, "actions/upload-artifact@") || step.With["if-no-files-found"] != "error" || step.With["retention-days"] != "30" || step.With["overwrite"] == "true" || step.If != "" || step.ContinueOnError {
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
