package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestManagedApprovedRepairFinishesRetainedPendingBuild(t *testing.T) {
	m := managedTestFixture(t)
	m.nativeFailure = "register resources"
	if err := m.run(t); err == nil {
		t.Fatal("expected interrupted installation")
	}
	i := m.installer
	pending := i.identity
	managedCandidate(t, m, "124")
	before := installTreeWitness(t, i.home)
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatal("ordinary installation must preserve pending identity", err)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("ordinary refusal mutated pending state")
	}
	m.nativeFailure = ""
	if err := i.repairAndExecute(context.Background()); err != nil {
		t.Fatal("approved recovery could not finish retained build before upgrading", err)
	}
	completed, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || completed.Build == nil || *completed.Build != 124 || completed.Equal(pending) {
		t.Fatal("new build did not complete after exact pending recovery", completed, err)
	}
	if _, err := os.Lstat(filepath.Join(i.root, "install-in-progress.json")); !os.IsNotExist(err) {
		t.Fatal("completed upgrade retained pending state", err)
	}
}

func TestManagedPendingBuild126NativeRefusalPreservesCustody(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	pending := managedCandidate(t, m, "126")
	m.nativeFailure = "register queue"
	if err := m.run(t); err == nil {
		t.Fatal("expected build 126 interruption")
	}
	backup, err := managedCaptureBundle(i.managedPreviousPath())
	if err != nil {
		t.Fatal(err)
	}
	managedCandidate(t, m, "127")
	m.nativeFailure = ""
	m.statuses["queue"] = "notFound"
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 4 && args[1] == "--service-management" && args[2] == "register" {
			return nil, errors.New("build 126 native bridge returned notFound without registration")
		}
		return original(ctx, args)
	}
	if err := i.repairAndExecute(context.Background()); err == nil || !strings.Contains(err.Error(), "custody were preserved") {
		t.Fatal("old native policy did not fail closed with a retained-state diagnostic", err)
	}
	actual, err := installReadInfo(filepath.Join(i.root, "install-in-progress.json"))
	if err != nil || !actual.Equal(pending) {
		t.Fatal("old native refusal lost pending build 126", actual, err)
	}
	if err := managedRequireBundle(i.managedPreviousPath(), backup); err != nil {
		t.Fatal("old native refusal lost the last-good backup", err)
	}
	current, err := managedCaptureBundle(i.app)
	if err != nil || !current.Build.Equal(pending) {
		t.Fatal("old native refusal published a different build", current, err)
	}
}

func TestManagedApprovedRepairPreservesUnrecoverablePending(t *testing.T) {
	for _, scenario := range []string{"unpublished", "failed-retry", "busy-queue", "replaced-app", "unknown-backup"} {
		t.Run(scenario, func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			m.nativeFailure = "register resources"
			if err := m.run(t); err == nil {
				t.Fatal("expected interrupted installation")
			}
			if scenario == "unpublished" {
				if err := os.RemoveAll(i.app); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "busy-queue" {
				m.nativeFailure = ""
				m.tasks["1"] = Object{"id": 1, "status": "Running"}
			}
			managedCandidate(t, m, "124")
			if scenario == "replaced-app" {
				if err := os.RemoveAll(i.app); err != nil {
					t.Fatal(err)
				}
				if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unknown-backup" {
				if err := installCopyTree(i.app, i.managedPreviousPath()); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile(filepath.Join(i.root, "install-in-progress.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := i.repairAndExecute(context.Background()); err == nil {
				t.Fatal("unresolved pending installation was replaced")
			}
			after, err := os.ReadFile(filepath.Join(i.root, "install-in-progress.json"))
			if err != nil || string(raw) != string(after) {
				t.Fatal("recovery lost original pending identity", err)
			}
		})
	}
}

func TestManagedApprovedRepairCompletesOwnedBackupBeforeUpgrade(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	managedCandidate(t, m, "124")
	m.nativeFailure = "register resources"
	if err := m.run(t); err == nil {
		t.Fatal("expected interrupted upgrade")
	}
	if _, err := os.Stat(i.managedPreviousPath()); err != nil {
		t.Fatal("missing retained last-good backup", err)
	}
	managedCandidate(t, m, "125")
	m.nativeFailure = ""
	if err := i.repairAndExecute(context.Background()); err != nil {
		t.Fatal("exact pending upgrade could not complete its backup custody", err)
	}
	completed, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || completed.Build == nil || *completed.Build != 125 {
		t.Fatal("new upgrade did not complete", completed, err)
	}
	if _, err := os.Lstat(i.managedPreviousPath()); !os.IsNotExist(err) {
		t.Fatal("completed upgrade did not retire owned backup", err)
	}
}

func TestManagedApprovedRepairFinishesRetainedLegacyBuild(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	app := filepath.Join(i.payload, "Agent Queue.app")
	info, err := updatePlist(filepath.Join(app, "Contents/Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	delete(info, "AgentQueueServiceManagement")
	info["AgentQueueEmbeddedPayload"] = true
	if err := i.writePlist(filepath.Join(app, "Contents/Info.plist"), info); err != nil {
		t.Fatal(err)
	}
	if err := installCopyTree(filepath.Join(i.payload, "source/bin"), filepath.Join(app, "Contents/Resources/payload/source/bin")); err != nil {
		t.Fatal(err)
	}
	m.fail = "io.went.agent-resources.plist"
	if err := m.run(t); err == nil {
		t.Fatal("expected interrupted legacy setup")
	}
	m.fail = ""
	info["AgentQueueServiceManagement"] = true
	if err := i.writePlist(filepath.Join(app, "Contents/Info.plist"), info); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(app, "Contents/Resources/payload/source/bin")); err != nil {
		t.Fatal(err)
	}
	managedCandidate(t, m, "124")
	if err := i.repairAndExecute(context.Background()); err != nil {
		t.Fatal("retained legacy setup could not finish before managed migration", err)
	}
	completed, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || completed.Build == nil || *completed.Build != 124 {
		t.Fatal("legacy recovery did not complete the managed upgrade", completed, err)
	}
}

func TestManagedPendingRecoveryRechecksItsSelectedMarker(t *testing.T) {
	m := managedTestFixture(t)
	m.nativeFailure = "register resources"
	if err := m.run(t); err == nil {
		t.Fatal("expected interrupted setup")
	}
	i := m.installer
	managedCandidate(t, m, "124")
	retained, err := i.pendingInstallation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(i.root, "install-in-progress.json")); err != nil {
		t.Fatal(err)
	}
	m.nativeFailure = ""
	before := installTreeWitness(t, i.home)
	m.calls = nil
	if err := retained.execute(context.Background()); err == nil || !strings.Contains(err.Error(), "pending installation changed") {
		t.Fatal("stale recovery selection was accepted", err)
	}
	managedRequireNoServiceMutation(t, m.calls)
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("stale recovery changed the installation")
	}
}
