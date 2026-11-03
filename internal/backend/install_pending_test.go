package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// A newer build dragged over T at the same path has replaced T's app, so nothing
// can finish T there. Its publication custody still refuses the takeover.
func TestManagedSamePathReplacementCannotTakeOverRetainedPublication(t *testing.T) {
	m := managedOlderRegistration(t)
	i := m.installer
	managedOlderPublication(t, m, "124")
	pending := integrationTestRead(t, filepath.Join(i.root, "install-in-progress.json"))
	managedCandidate(t, m, "125")
	before := installTreeWitness(t, i.home)
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatal("ordinary installation must preserve pending identity", err)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("ordinary refusal mutated pending state")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := i.repairAndExecute(ctx); err == nil {
		t.Fatal("a replaced retained build was treated as finished")
	}
	if after := integrationTestRead(t, filepath.Join(i.root, "install-in-progress.json")); string(after) != string(pending) {
		t.Fatal("recovery lost the original pending identity")
	}
	if _, err := os.Stat(i.managedReceiptPath()); err != nil {
		t.Fatal("recovery lost publication custody", err)
	}
}

func TestManagedPendingBuild126NativeRefusalPreservesCustody(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := managedOlderRegistered(ctx, m); err != nil {
		t.Fatal(err)
	}
	backup, _ := managedOlderPublication(t, m, "126")
	pending, err := installReadInfo(filepath.Join(i.root, "install-in-progress.json"))
	if err != nil {
		t.Fatal(err)
	}
	retained := i.app
	previous := i.managedPreviousPath()
	dropped := filepath.Join(i.home, "Applications/Agent Queue dragged.app")
	if err := installCopyTree(i.app, dropped); err != nil {
		t.Fatal(err)
	}
	managedBundleBuild(t, i, dropped, "127")
	i.app = dropped
	m.statuses["login"] = "notFound"
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
	if err := managedRequireBundle(previous, backup); err != nil {
		t.Fatal("old native refusal lost the last-good backup", err)
	}
	current, err := managedCaptureBundle(retained)
	if err != nil || !current.Build.Equal(pending) {
		t.Fatal("old native refusal published a different build", current, err)
	}
}

func TestManagedApprovedRepairPreservesUnrecoverablePending(t *testing.T) {
	for _, scenario := range []string{"failed-retry", "busy-queue"} {
		t.Run(scenario, func(t *testing.T) {
			m, _ := managedRelocationFixture(t, true)
			i := m.installer
			if scenario == "failed-retry" {
				m.nativeFailure = "register login"
			} else {
				m.tasks["1"] = Object{"id": 1, "status": Object{"Running": Object{}}}
			}
			raw, err := os.ReadFile(filepath.Join(i.root, "install-in-progress.json"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			if err := i.repairAndExecute(ctx); err == nil {
				t.Fatal("unresolved pending installation was replaced")
			}
			after, err := os.ReadFile(filepath.Join(i.root, "install-in-progress.json"))
			if err != nil || string(raw) != string(after) {
				t.Fatal("recovery lost original pending identity", err)
			}
		})
	}
}

func TestManagedAutomaticRetainedCandidateMigratesOldApprovalThenUpgrades(t *testing.T) {
	m, _ := managedRelocationFixture(t, true)
	i := m.installer
	m.statuses["resources"] = "requiresApproval"
	m.calls = nil
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	// Recovery of the retained candidate retires the registration awaiting
	// approval instead of waiting for consent that app-bundle setup never needs.
	if err := i.repairAndExecute(ctx); err != nil {
		t.Fatal("retained approval blocked recovery", err)
	}
	completed, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || completed.Build == nil || *completed.Build != 124 {
		t.Fatal("new candidate did not follow the retained candidate", completed, err)
	}
	if !slices.Contains(managedServiceCalls(m, "unregister"), "resources") {
		t.Fatal("the registration awaiting approval was not retired", m.calls)
	}
}

func TestManagedPendingRecoveryRechecksItsSelectedMarker(t *testing.T) {
	m, _ := managedRelocationFixture(t, true)
	i := m.installer
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
