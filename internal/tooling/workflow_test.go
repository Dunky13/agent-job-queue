package tooling

import (
	"os"
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
