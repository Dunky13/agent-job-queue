package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagedSetupStopRetainsAcceptedMutation(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	stop := make(chan struct{})
	i.setupStop = stop
	original := i.command
	requested := false
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 4 && args[1] == "--service-management" && args[2] == "register" {
			if !requested {
				close(stop)
				requested = true
			}
			// Yield to the cooperative stop receiver before checking the context.
			time.Sleep(5 * time.Millisecond)
			if err := ctx.Err(); err != nil {
				t.Fatal("a late stop request cancelled an accepted registration", err)
			}
		}
		return original(ctx, args)
	}
	if err := i.repairAndExecute(context.Background()); err != nil {
		t.Fatal("accepted installation did not reach its original response", err)
	}
	if !requested {
		t.Fatal("fixture did not request stopping during mutation")
	}
	if _, err := installReadInfo(filepath.Join(i.root, "build-info.json")); err != nil {
		t.Fatal("accepted installation was not completed", err)
	}
}

func TestManagedSetupStopReachesNestedPendingApproval(t *testing.T) {
	m := managedTestFixture(t)
	m.nativeFailure = "register resources"
	if err := m.run(t); err == nil {
		t.Fatal("expected an interrupted retained candidate")
	}
	i := m.installer
	pending := i.identity
	managedCandidate(t, m, "124")
	m.nativeFailure, m.approval = "", "resources"
	stop := make(chan struct{})
	i.setupStop = stop
	original := i.command
	requested := false
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		raw, err := original(ctx, args)
		if !requested && len(args) == 4 && args[1] == "--service-management" && args[2] == "status" && args[3] == "resources" && m.statuses["resources"] == "requiresApproval" {
			requested = true
			close(stop)
		}
		return raw, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := i.repairAndExecute(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("stop did not reach the nested retained-candidate wait", err)
	}
	actual, err := installReadInfo(filepath.Join(i.root, "install-in-progress.json"))
	if err != nil || !actual.Equal(pending) {
		t.Fatal("stopping lost the original pending identity", actual, err)
	}
	if _, err := os.Stat(i.managedReceiptPath()); err != nil {
		t.Fatal("stopping lost original publication custody", err)
	}
}
