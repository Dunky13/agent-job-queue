package tooling

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A plain app cask installs nothing but the app: setup runs when a person opens
// it, so the real Homebrew DSL must see no installer, hook or command link.
func TestRenderedCaskHasOnlyAppAndQuitArtifacts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("real Homebrew DSL requires macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is unavailable")
	}
	fixture, _, environment := caskFixture(t)
	cask := fixtureCask(t, fixture, "0.1.0", 123)
	output, err := runBrewRuby(t, 30*time.Second, environment, brew, "--", "../../packaging/homebrew/test-cask.rb", cask)
	if err != nil {
		t.Fatalf("rendered cask was rejected by the delivery contract: %v: %s", err, output)
	}
	var result struct {
		Version  string   `json:"version"`
		URL      string   `json:"url"`
		Order    []string `json:"artifact_order"`
		Installs bool     `json:"fixture_app_installed"`
		App      struct {
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"app"`
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("invalid cask evidence: %v: %s", err, output)
	}
	if result.Version != "0.1.0,123" || !strings.HasSuffix(result.URL, "/build-123-aaaaaaa/Agent-Queue-0.1.0-universal.zip") || result.Installs {
		t.Fatalf("unexpected release identity: %+v", result)
	}
	if strings.Join(result.Order, ",") != "Cask::Artifact::Uninstall,Cask::Artifact::App" {
		t.Fatalf("the cask must contain only its quit-only uninstall and app: %v", result.Order)
	}
	if !strings.HasSuffix(result.App.Source, "/Agent Queue.app") || !strings.HasSuffix(result.App.Target, "/Agent Queue.app") {
		t.Fatalf("the app must keep its own name in the Applications directory: %+v", result.App)
	}
}
