package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInstallerPreflightReadinessCannotPreparePrivateState(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(f.installer.root, "bin/agent-heavy")
	if err := os.WriteFile(binary, raw, 0700); err != nil {
		t.Fatal(err)
	}
	original := f.installer.command
	statusCalls := 0
	f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[0] == binary && args[1] == "status" {
			statusCalls++
			command := exec.CommandContext(ctx, args[0], args[1:]...)
			command.Env = f.installer.commandEnvironment(args)
			return command.CombinedOutput()
		}
		return original(ctx, args)
	}
	before := installTreeWitness(t, f.installer.home)
	_ = f.installer.preflight(context.Background())
	if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.home)) {
		t.Fatal("preflight readiness changed private state through the real status CLI")
	}
	if statusCalls != 0 {
		t.Fatal("preflight invoked the state-preparing status command")
	}
}

func TestInstallerPreflightLetsNormalInstallRepairNonExecutableBackend(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	delete(f.loaded, "io.went.agent-heavy")
	binary := filepath.Join(f.installer.root, "bin/agent-heavy")
	if err := os.Chmod(binary, 0600); err != nil {
		t.Fatal(err)
	}
	report := f.installer.preflight(context.Background())
	if preflightIDs(report)["service-not-ready"] {
		t.Fatal("preflight blocks payload repair by offering startup of a non-executable backend")
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(binary)
	if err != nil || info.Mode().Perm()&0100 == 0 {
		t.Fatal("normal install did not repair executable permissions", err)
	}
}

func TestInstallerRecoveryRejectsSymlinkedExecutableParent(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	delete(f.loaded, "io.went.agent-heavy")
	bin := filepath.Join(f.installer.root, "bin")
	if err := os.Rename(bin, bin+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin+"-original", bin); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := f.installer.recover(context.Background(), "repair-service", nil, ""); err == nil {
		t.Fatal("recovery started an executable through a symlinked parent")
	}
	for _, args := range f.calls {
		if len(args) > 1 && args[0] == "launchctl" && args[1] == "bootstrap" {
			t.Fatal("unsafe executable parent authorized service bootstrap")
		}
	}
}

func TestInstallerPreflightExplainsLegacyStateWithoutConfiguration(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	if err := os.Remove(filepath.Join(f.installer.root, "agent_heavy.py")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.installer.config, "pueue.yml")); err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(f.installer.state, "pueue/state.json"), mustMetadataJSON(t, f.snapshot()))
	report := f.installer.preflight(context.Background())
	if !preflightIDs(report)["configuration-missing"] {
		t.Fatal("retained legacy state received no configuration recovery guidance")
	}
}

func TestInstallerPreflightSuppressesJobsBlockedByCustomCallback(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	f.tasks = Object{"7": Object{"id": 7, "status": Object{"Queued": Object{}}}}
	configuration := f.installer.freshConfig()
	configuration["daemon"].(Object)["callback"] = "custom-hook {{ id }}"
	integrationTestWrite(t, filepath.Join(f.installer.config, "pueue.yml"), mustMetadataJSON(t, configuration))
	integrationTestWrite(t, filepath.Join(f.installer.state, "receipts/8.json"), []byte(`{"task_id":8,"token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
	report := f.installer.preflight(context.Background())
	for _, issue := range report.Issues {
		for _, action := range issue.Actions {
			if action.ID == "cancel-job" || action.ID == "cleanup-job" {
				t.Fatal("custom callback report offers a permanently ineligible job recovery")
			}
		}
	}
	if _, err := os.Stat(filepath.Join(f.installer.state, "receipts/8.json")); errors.Is(err, os.ErrNotExist) {
		t.Fatal("receipt lost")
	}
}

func TestInstallerPreflightOnlyOffersCleanupForCompletedJobs(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	f.tasks = Object{
		"7": Object{"id": 7, "status": Object{"Running": Object{}}},
		"8": Object{"id": 8, "status": Object{"Done": Object{"result": "Success"}}},
	}
	for _, id := range []string{"7", "8", "9"} {
		integrationTestWrite(t, filepath.Join(f.installer.state, "receipts", id+".json"), []byte(`{"token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
	}
	report := f.installer.preflight(context.Background())
	for _, issue := range report.Issues {
		if issue.ID == "pending-cleanup" {
			if len(issue.Jobs) != 1 || issue.Jobs[0].ID != 8 {
				t.Fatal("cleanup includes active or missing tasks", issue.Jobs)
			}
			return
		}
	}
	t.Fatal("pending cleanup issue missing")
}
