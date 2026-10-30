package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
)

// Both bundles live in private allowed Applications paths. Track the actual
// registered location independently of the installer's newly selected target.
// No native bridge or installed service is invoked by this fixture.
func managedRelocationFixture(t *testing.T, pending bool, backup ...bool) (*managedFixture, string) {
	t.Helper()
	m := managedTestFixture(t)
	i := m.installer
	old := i.app
	newBuild := "124"
	if len(backup) > 0 && backup[0] {
		if err := m.run(t); err != nil {
			t.Fatal(err)
		}
		managedCandidate(t, m, "124")
		newBuild = "125"
	}
	if pending {
		m.nativeFailure = "register resources"
	}
	err := m.run(t)
	if pending && err == nil || !pending && err != nil {
		t.Fatal("unexpected initial installation result", err)
	}
	m.nativeFailure = ""
	services := map[string]string{}
	for _, service := range []string{"queue", "resources", "remote"} {
		services[service] = old
	}
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[0] == filepath.Join(old, "Contents/MacOS/agent-heavy") && args[1] == "status" {
			m.calls = append(m.calls, slices.Clone(args))
			return mustMetadataJSON(t, Object{"updatedAt": time.Now().UTC().Format(time.RFC3339Nano), "queue": Object{"status": "running", "limit": 1, "running": 0, "waiting": 0}, "jobs": []any{}, "scheduler": Object{}}), nil
		}
		if len(args) == 3 && args[0] == "launchctl" && args[1] == "print" {
			label := filepath.Base(args[2])
			service := map[string]string{"io.went.agent-heavy": "queue", "io.went.agent-resources": "resources", "io.went.agent-remote": "remote"}[label]
			if service != "" && m.loaded[label] && m.statuses[service] == "enabled" {
				m.calls = append(m.calls, slices.Clone(args))
				helper, command := "agent-heavy", "daemon"
				if service == "resources" {
					command = "scheduler"
				} else if service == "remote" {
					helper, command = "agent-remote", "serve"
				}
				return []byte("program = " + filepath.Join(services[service], "Contents/MacOS", helper) + "\narguments = {\n" + helper + "\n" + command + "\n}\n"), nil
			}
		}
		if len(args) == 4 && args[1] == "--service-management" && args[2] == "register" {
			services[args[3]] = filepath.Dir(filepath.Dir(filepath.Dir(args[0])))
		}
		return original(ctx, args)
	}
	managedCandidate(t, m, newBuild)
	dropped := filepath.Join(i.home, "Applications/Agent Queue dragged.app")
	if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), dropped); err != nil {
		t.Fatal(err)
	}
	i.app, i.appBundle, i.payload = dropped, true, filepath.Join(dropped, "Contents/Resources/payload")
	m.calls = nil
	return m, old
}

func TestManagedDraggedAppFinishesRecordedPendingBeforeRelocation(t *testing.T) {
	m, old := managedRelocationFixture(t, true)
	i := m.installer
	retained, err := managedCaptureBundle(old)
	if err != nil {
		t.Fatal(err)
	}
	report := i.preflight(context.Background())
	if len(report.Issues) != 1 || report.Issues[0].ID != "pending-installation" || !slices.ContainsFunc(report.Issues[0].Actions, func(action installRecoveryAction) bool { return action.ID == installFinishPending }) {
		t.Fatalf("retained exact app has no usable recovery: %+v", report)
	}
	completedOld := false
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 4 && args[1] == "--service-management" && args[2] == "register" && strings.HasPrefix(args[0], i.app+"/") {
			info, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
			if err != nil || !info.Equal(retained.Build) {
				t.Fatal("new location activated before exact retained build completed", info, err)
			}
			completedOld = true
		}
		return original(ctx, args)
	}
	if err := i.repairAndExecute(context.Background()); err != nil {
		t.Fatal("normal launch could not recover and relocate", err)
	}
	if !completedOld {
		t.Fatal("new bundle was never registered")
	}
	if err := managedRequireBundle(old, retained); err != nil {
		t.Fatal("the old user bundle was removed or changed", err)
	}
	metadata, err := appservices.Load(i.home)
	if err != nil || metadata == nil || metadata.App != i.app {
		t.Fatal("new location was not published", metadata, err)
	}
	completed, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || completed.Build == nil || *completed.Build != 124 {
		t.Fatal("new build was not completed", completed, err)
	}
}

func TestManagedRelocationWaitsWithoutChangingRunningJobs(t *testing.T) {
	m, old := managedRelocationFixture(t, false)
	i := m.installer
	m.tasks["7"] = Object{"id": 7, "status": Object{"Running": Object{}}}
	before := installTreeWitness(t, i.home)
	err := m.run(t)
	var drain *installDrainPendingError
	if !errors.As(err, &drain) {
		t.Fatal("relocation did not reach its owned queue drain gate", err)
	}
	managedRequireNoServiceMutation(t, m.calls)
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("waiting changed installation state")
	}
	delete(m.tasks, "7")
	if err := i.repairAndExecute(context.Background()); err != nil {
		t.Fatal("drained relocation could not continue", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("old user app was not preserved", err)
	}
}

func TestManagedRecordedPendingRefusesChangedOrUnsafeApp(t *testing.T) {
	for _, scenario := range []string{"missing", "changed-build", "substituted-inode", "unsafe-path", "unsafe-permissions", "bad-signature", "unowned-backup", "unrelated-loaded-service"} {
		t.Run(scenario, func(t *testing.T) {
			m, old := managedRelocationFixture(t, true)
			i := m.installer
			switch scenario {
			case "missing":
				if err := os.Rename(old, old+".retained"); err != nil {
					t.Fatal(err)
				}
			case "changed-build":
				integrationTestWrite(t, filepath.Join(old, "Contents/Resources/payload/source/build-info.json"), mustMetadataJSON(t, metadataTestInfo(t, "125")))
			case "substituted-inode":
				if err := os.Rename(old, old+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := installCopyTree(old+".retained", old); err != nil {
					t.Fatal(err)
				}
			case "unsafe-path":
				if err := os.Rename(old, old+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(old+".retained", old); err != nil {
					t.Fatal(err)
				}
			case "unsafe-permissions":
				if err := os.Chmod(filepath.Join(old, "Contents/MacOS/agent-heavy"), 0777); err != nil {
					t.Fatal(err)
				}
			case "bad-signature":
				original := i.command
				i.command = func(ctx context.Context, args []string) ([]byte, error) {
					if args[0] == "/usr/bin/codesign" && strings.HasPrefix(args[len(args)-1], old) {
						return nil, errors.New("fixture invalid signature")
					}
					return original(ctx, args)
				}
			case "unowned-backup":
				if err := installCopyTree(old, i.managedPreviousPath()); err != nil {
					t.Fatal(err)
				}
			case "unrelated-loaded-service":
				original := i.command
				i.command = func(ctx context.Context, args []string) ([]byte, error) {
					if len(args) == 3 && args[0] == "launchctl" && args[1] == "print" && strings.HasSuffix(args[2], "/io.went.agent-heavy") {
						return []byte("program = /unrelated/helper\narguments = {\nagent-heavy\ndaemon\n}\n"), nil
					}
					return original(ctx, args)
				}
			}
			before := installTreeWitness(t, i.home)
			if err := i.repairAndExecute(context.Background()); err == nil {
				t.Fatal("unsafe retained candidate was accepted")
			}
			managedRequireNoServiceMutation(t, m.calls)
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("refusal changed pending identity, custody or app bytes")
			}
			report := i.preflight(context.Background())
			if len(report.Issues) == 0 {
				t.Fatal("unsafe recovery lost its actionable requirement")
			}
			if scenario != "unrelated-loaded-service" && !slices.ContainsFunc(report.Issues[0].Actions, func(action installRecoveryAction) bool { return action.ID == "download-release" }) {
				t.Fatal("unrecoverable candidate has no release-download action", report)
			}
		})
	}
}

func TestManagedPendingSelectionPinsRecordedBundle(t *testing.T) {
	m, old := managedRelocationFixture(t, true)
	retained, err := m.installer.pendingInstallation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, old+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := installCopyTree(old+".retained", old); err != nil {
		t.Fatal(err)
	}
	before := installTreeWitness(t, m.installer.home)
	m.calls = nil
	if err := retained.execute(context.Background()); err == nil {
		t.Fatal("same-content replacement bypassed selected pending custody")
	}
	managedRequireNoServiceMutation(t, m.calls)
	if !reflect.DeepEqual(before, installTreeWitness(t, m.installer.home)) {
		t.Fatal("changed pending selection mutated retained evidence")
	}
}

func TestManagedDraggedAppRecoversRecordedPublicationBackup(t *testing.T) {
	m, old := managedRelocationFixture(t, true, true)
	i := m.installer
	retained, err := managedCaptureBundle(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(i.managedPreviousPath()); err != nil {
		t.Fatal("fixture did not retain a last-good backup", err)
	}
	if err := i.repairAndExecute(context.Background()); err != nil {
		t.Fatal("recorded pending publication could not finish before relocation", err)
	}
	if err := managedRequireBundle(old, retained); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{i.managedPreviousPath(), i.managedReceiptPath(), filepath.Join(i.root, "install-in-progress.json")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("completed recovery did not retire exact owned publication custody", path, err)
		}
	}
}

func TestManagedDistinctAppDoesNotSupersedeRecordedPendingWithoutReceipt(t *testing.T) {
	m, _ := managedRelocationFixture(t, true)
	i := m.installer
	if err := os.Remove(i.managedReceiptPath()); err != nil {
		t.Fatal(err)
	}
	var pending *installPendingError
	if err := i.validatePayload(); !errors.As(err, &pending) {
		t.Fatal("a different app superseded the still-retained exact pending build", err)
	}
	if err := i.repairAndExecute(context.Background()); err != nil {
		t.Fatal("repeatable retained setup without publication custody could not finish", err)
	}
}

func TestManagedRelocationKeepsRetainedCallerUntilItExits(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("runtime process birth checks require macOS")
	}
	m, oldApp := managedRelocationFixture(t, false)
	i := m.installer
	old := *i
	old.app, old.payload = oldApp, filepath.Join(oldApp, "Contents/Resources/payload")
	attempt := runtimeGenerationAttemptFixture(t)
	caller, identity := runtimeGenerationCallerFixture(t)
	attempt.Caller, attempt.Worker = identity, nil
	source, err := managedCaptureBundle(oldApp)
	if err != nil {
		t.Fatal(err)
	}
	retainedApp := filepath.Join(old.runtimeGenerationDirectory(), "fixture.app")
	if err := installCopyTree(oldApp, retainedApp); err != nil {
		t.Fatal(err)
	}
	retained, err := managedCaptureBundle(retainedApp)
	if err != nil {
		t.Fatal(err)
	}
	path := old.runtimeGenerationPath(retained)
	if err := os.Rename(retainedApp, path); err != nil {
		t.Fatal(err)
	}
	record := managedRuntimeGeneration{SchemaVersion: 1, App: oldApp, Path: path, Phase: "retained", Runtime: retained, Candidate: source, Attempts: []runtimeGenerationAttempt{attempt}}
	integrationTestWrite(t, old.runtimeGenerationRecordPath(retained), mustMetadataJSON(t, record))
	store := runtimeGenerationJob(t, i, attempt, "done")
	store.close()
	if err := m.run(t); !errors.Is(err, errInstallRelocationWaiting) {
		t.Fatal("relocation did not wait for the original terminal job's caller", err)
	}
	managedRequireNoServiceMutation(t, m.calls)
	if _, err := os.Lstat(filepath.Join(i.root, "install-in-progress.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("waiting relocation published a new pending identity", err)
	}
	if err := managedRequireBundle(path, retained); err != nil {
		t.Fatal("caller lost its executable custody", err)
	}
	if err := caller.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = caller.Wait()
	if err := i.repairAndExecute(context.Background()); err != nil {
		t.Fatal("relocation could not reclaim the exact finished generation and continue", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("finished installer-owned generation was not reclaimed", err)
	}
	if err := managedRequireBundle(oldApp, source); err != nil {
		t.Fatal("relocation removed the user's old bundle", err)
	}
}

func TestManagedAutomaticRelocationWaitsThroughFinalCallerReadback(t *testing.T) {
	executions, waits := 0, 0
	state := ""
	err := installRunApprovedRepair(context.Background(), func(context.Context) installPreflightReport {
		return approvedTestReport()
	}, func(context.Context, string, *int, string) error {
		t.Fatal("unexpected repair action")
		return nil
	}, func(context.Context) error {
		if state != "AGENT_QUEUE_SETUP_STATE:installing" {
			t.Fatal("mutation did not revoke cancellable waiting", state)
		}
		executions++
		if executions < 3 {
			return errInstallRelocationWaiting
		}
		return nil
	}, func(context.Context) error {
		if state != "AGENT_QUEUE_SETUP_STATE:waiting" {
			t.Fatal("retained-caller waiting was not exposed to the native client", state)
		}
		waits++
		return nil
	}, func(message string) { state = strings.SplitN(message, "\n", 2)[0] })
	if err != nil || executions != 3 || waits != 2 {
		t.Fatal("automatic relocation did not resume after retained caller drain", executions, waits, err)
	}
}

func TestManagedRecordedPendingDrainsAndResumesApprovalBeforeRelocation(t *testing.T) {
	m, old := managedRelocationFixture(t, true)
	i := m.installer
	m.tasks["7"] = Object{"id": 7, "status": Object{"Running": Object{}}}
	reads := 0
	i.queueSnapshot = func(context.Context) (Object, error) {
		reads++
		if reads == 3 {
			delete(m.tasks, "7")
		}
		return m.snapshot(), nil
	}
	m.approval = "resources"
	approvalReads := 0
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 4 && args[1] == "--service-management" && args[2] == "status" && args[3] == "resources" && m.statuses["resources"] == "requiresApproval" {
			approvalReads++
			if approvalReads == 3 {
				m.approval = ""
				m.statuses["resources"] = "enabled"
				m.loaded["io.went.agent-resources"] = true
			}
		}
		if len(args) == 4 && args[1] == "--service-management" && args[2] != "status" && len(m.tasks) != 0 {
			t.Fatal("pending recovery changed a service before the old jobs drained")
		}
		return original(ctx, args)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := i.repairAndExecute(ctx); err != nil {
		t.Fatal("automatic recovery failed to resume the recorded candidate after drain and approval", err)
	}
	if approvalReads < 3 {
		t.Fatal("fixture did not exercise an approval pause")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("completed relocation removed the old app", err)
	}
}
