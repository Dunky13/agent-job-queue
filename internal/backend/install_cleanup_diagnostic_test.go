package backend

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstallerPreflightCleanupDiagnosticIncludesRunningWork(t *testing.T) {
	for _, kind := range []string{"running-receipt", "running-marker", "running-external-marker", "completed-receipt"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			status := Object{"Running": Object{}}
			if kind == "completed-receipt" {
				status = Object{"Done": Object{"result": "Success"}}
			}
			f.tasks = Object{"7": Object{"id": 7, "label": "Fixture work", "status": status}}
			if kind == "running-external-marker" {
				integrationTestWrite(t, filepath.Join(f.installer.state, "legacy-slot.json"), mustMetadataJSON(t, Object{"slot": filepath.Join(filepath.Dir(f.installer.home), "external-slot")}))
			}
			if kind == "running-marker" || kind == "running-external-marker" {
				slot, err := f.installer.configuredSlot()
				if err != nil {
					t.Fatal(err)
				}
				integrationTestWrite(t, filepath.Join(slot, "agent-heavy.json"), []byte(`{"task_id":7}`))
			} else {
				integrationTestWrite(t, filepath.Join(f.installer.state, "receipts/7.json"), []byte(`{"task_id":7,"token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
			}
			before := installTreeWitness(t, filepath.Dir(f.installer.home))
			pending := f.installer.requireLegacyCleanup()
			if pending == nil {
				t.Fatal("retained work or cleanup no longer blocks migration")
			}
			report := f.installer.preflight(context.Background())
			var issue *installPreflightIssue
			for i := range report.Issues {
				if report.Issues[i].ID == "pending-cleanup" {
					issue = &report.Issues[i]
				}
			}
			if issue == nil {
				t.Fatalf("work or cleanup blocker missing: %+v", report)
			}
			expectedPath := filepath.Join(f.installer.state, "receipts")
			if kind == "running-marker" || kind == "running-external-marker" {
				slot, err := f.installer.configuredSlot()
				if err != nil {
					t.Fatal(err)
				}
				expectedPath = filepath.Join(slot, "agent-heavy.json")
			}
			if issue.Path != expectedPath {
				t.Errorf("cleanup blocker points to unrelated state: got %s, want %s", issue.Path, expectedPath)
			}
			if issue.Title != "Wait for queue work and cleanup" || !strings.Contains(issue.Detail, "running") || !strings.Contains(pending.Error(), "running") {
				t.Errorf("diagnostic mislabels retained active work as completed cleanup: title=%q, detail=%q, error=%q", issue.Title, issue.Detail, pending)
			}
			wait, cleanup, reveal := false, false, false
			for _, action := range issue.Actions {
				wait = wait || action.ID == "wait"
				cleanup = cleanup || action.ID == "cleanup-job"
				reveal = reveal || action.ID == "reveal"
			}
			if reveal != (kind != "running-external-marker") {
				t.Errorf("file reveal does not respect the native account-folder boundary: %+v", issue.Actions)
			}
			if kind == "running-external-marker" && !strings.Contains(issue.Detail, "manually") {
				t.Error("external marker has no manual inspection guidance")
			}
			if !wait {
				t.Error("work or cleanup blocker lost its wait action")
			}
			if kind == "completed-receipt" {
				if !cleanup || len(issue.Jobs) != 1 || issue.Jobs[0].ID != 7 {
					t.Errorf("eligible completed job lost verified cleanup: %+v", issue)
				}
			} else if cleanup || len(issue.Jobs) != 0 {
				t.Errorf("running job offered completed-job cleanup: %+v", issue)
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, filepath.Dir(f.installer.home))) {
				t.Fatal("cleanup inspection changed retained queue files")
			}
			for _, args := range f.calls {
				if len(args) > 1 && (args[1] == "bootout" || args[1] == "bootstrap" || args[1] == "disable") {
					t.Fatalf("cleanup inspection mutated service: %v", args)
				}
			}
		})
	}
}
