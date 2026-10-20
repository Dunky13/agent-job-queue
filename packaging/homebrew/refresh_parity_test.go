package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dunky13/agent-job-queue/internal/backend"
	"github.com/Dunky13/agent-job-queue/internal/tooling"
)

// These source-only checks stay in the app module. The tap receives only the
// standalone helper and refresh_test.go, which must not import app packages.
func TestRefreshCaskMatchesRuntimeRendererWithoutRuntimePrerequisites(t *testing.T) {
	data := fixtureManifest(1)
	standalone, err := renderCask(data)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := tooling.RenderCask(backend.Manifest{BuildInfo: backend.BuildInfo{SchemaVersion: data.SchemaVersion, Repository: data.Repository, Version: data.Version, Commit: &data.Commit, Build: &data.Build}, Tag: data.Tag, Asset: data.Asset, SHA256: data.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	if standalone != runtime {
		t.Fatal("standalone tap refresher and app release renderer generated different casks")
	}
	if strings.Contains(standalone, "depends_on formula:") {
		t.Fatal("cask must not require an external runtime formula")
	}
}

func TestRefreshStandaloneFilesRunOutsideAppModule(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"refresh.go", "refresh_test.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("go", "test", "-p", "2", "refresh.go", "refresh_test.go")
	command.Dir = directory
	command.Env = append(os.Environ(), "GOWORK=off", "GO111MODULE=on", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("copied tap tests require the source module or network: %v\n%s", err, output)
	}
}
