package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestInstallerPreflightReadinessCannotPreparePrivateState(t *testing.T) {
	f := installTestFixture(t)
	installOlderEmbeddedBlocked(t, f.installer)
	f.loaded["io.went.agent-heavy"] = true
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
	installOlderEmbeddedBlocked(t, f.installer)
	binary := filepath.Join(f.installer.root, "bin/agent-heavy")
	if err := os.Chmod(binary, 0600); err != nil {
		t.Fatal(err)
	}
	report := f.installer.preflight(context.Background())
	if preflightIDs(report)["service-not-ready"] {
		t.Fatal("preflight blocks setup by offering startup of a non-executable backend")
	}
	if err := os.Remove(filepath.Join(f.installer.state, "receipts/9.json")); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(binary); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("normal install did not retire the copied executable", err)
	}
}

func TestInstallerRecoveryRejectsSymlinkedExecutableParent(t *testing.T) {
	f := installTestFixture(t)
	installOlderEmbeddedBlocked(t, f.installer)
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
	report := f.installer.preflightLegacy(context.Background(), installPreflightReport{SchemaVersion: 1, Issues: []installPreflightIssue{}})
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
	integrationTestWrite(t, filepath.Join(f.installer.config, "remote.json"), []byte(`{"enabled":true}`))
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

// Nothing in an invalid bundle can be trusted, so preflight reviews the existing
// installation without it.
func TestInstallerPreflightReviewsStateBesideAnInvalidBundle(t *testing.T) {
	for _, kind := range []string{"outside Applications", "unmarked"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			i := f.installer
			f.legacy(t)
			integrationTestWrite(t, filepath.Join(i.config, "remote.json"), []byte(`{"enabled":true}`))
			if kind == "unmarked" {
				info, err := updatePlist(filepath.Join(i.app, "Contents/Info.plist"))
				if err != nil {
					t.Fatal(err)
				}
				delete(info, "AgentQueueServiceManagement")
				if err := i.writePlist(filepath.Join(i.app, "Contents/Info.plist"), info); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(filepath.Join(i.home, "Downloads"), 0700); err != nil {
					t.Fatal(err)
				}
				installMoveBundle(t, i, filepath.Join(i.home, "Downloads/Agent Queue.app"))
			}
			f.calls = nil
			report := i.preflight(context.Background())
			ids := preflightIDs(report)
			if !ids["payload-invalid"] || !ids["legacy-phone-access"] || ids["managed-setup-blocked"] {
				t.Fatalf("invalid bundle did not get the detailed review: %+v", report)
			}
			wantMove := kind != "unmarked"
			if got := report.Issues[0].Actions[0].ID == "move-app"; got != wantMove {
				t.Fatalf("move action %v, want %v: %+v", got, wantMove, report.Issues[0])
			}
			managedRequireNoServiceMutation(t, f.calls)
			for _, args := range f.calls {
				if args[0] == "/usr/bin/codesign" || len(args) > 1 && args[1] == "--service-management" {
					t.Fatal("preflight used an invalid bundle", args)
				}
			}
		})
	}
}

func TestInstallerPreflightLegacyDiagnostics(t *testing.T) {
	failing := func(f *installFixture, match func([]string) bool, output string) {
		original := f.installer.command
		f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
			if match(args) {
				if output == "" {
					return nil, errors.New("inspection timeout")
				}
				return []byte(output), nil
			}
			return original(ctx, args)
		}
	}
	print := func(label string) func([]string) bool {
		return func(args []string) bool {
			return len(args) == 3 && args[0] == "launchctl" && args[1] == "print" && strings.HasSuffix(args[2], "/"+label)
		}
	}
	const foreign = "program = /unknown\narguments = {\n/unknown\n}\n"
	for name, test := range map[string]struct {
		prepare func(t *testing.T, f *installFixture)
		title   string
		only    bool
	}{
		"unsafe private path": {func(t *testing.T, f *installFixture) {
			i := f.installer
			if err := os.Rename(i.state, i.state+".retained"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(i.state+".retained", i.state); err != nil {
				t.Fatal(err)
			}
		}, "Review private installation permissions", true},
		"installer busy":  {func(t *testing.T, f *installFixture) { holdInstallLock(t, f.installer.state, "install.lock") }, "Another installation is running", false},
		"scheduling busy": {func(t *testing.T, f *installFixture) { holdInstallLock(t, f.installer.state, "scheduling.lock") }, "A queue change is running", false},
		"remote busy": {func(t *testing.T, f *installFixture) {
			holdInstallLock(t, filepath.Join(f.installer.state, "remote"), "control.lock")
		}, "Phone access settings are changing", false},
		"invalid configuration": {func(t *testing.T, f *installFixture) {
			integrationTestWrite(t, filepath.Join(f.installer.config, "pueue.yml"), []byte("daemon: [unterminated"))
		}, "Review queue configuration", false},
		"customized service": {func(t *testing.T, f *installFixture) {
			if err := f.installer.writePlist(filepath.Join(f.installer.launch, "io.went.agent-resources.plist"), Object{"Label": "io.went.agent-resources", "ProgramArguments": []string{"/custom/controller"}}); err != nil {
				t.Fatal(err)
			}
		}, "Review background service configuration", false},
		"loaded service differs": {func(t *testing.T, f *installFixture) {
			failing(f, print("io.went.agent-resources"), foreign)
		}, "Review loaded background service ownership", false},
		"queue inspection fails": {func(t *testing.T, f *installFixture) {
			failing(f, print("io.went.agent-heavy"), "")
		}, "Cannot inspect the old queue service", false},
		"loaded queue changes": {func(t *testing.T, f *installFixture) {
			original := f.installer.command
			reads := 0
			f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
				if print("io.went.agent-heavy")(args) {
					// Finding missing definitions and checking loaded services read it first.
					if reads++; reads > 2 {
						return []byte(foreign), nil
					}
				}
				return original(ctx, args)
			}
		}, "Review loaded queue ownership", false},
		"phone inspection fails": {func(t *testing.T, f *installFixture) {
			failing(f, print("io.went.agent-remote"), "")
		}, "Cannot inspect phone access service", false},
		"loaded phone differs": {func(t *testing.T, f *installFixture) {
			f.loaded["io.went.agent-remote"] = true
			if err := f.installer.writePlist(filepath.Join(f.installer.launch, "io.went.agent-remote.plist"), Object{"Label": "io.went.agent-remote", "ProgramArguments": []string{"/custom/phone"}}); err != nil {
				t.Fatal(err)
			}
		}, "Review loaded phone access ownership", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			if err := os.MkdirAll(filepath.Join(f.installer.state, "remote"), 0700); err != nil {
				t.Fatal(err)
			}
			test.prepare(t, f)
			before := installTreeWitness(t, f.installer.home)
			report := f.installer.preflightLegacy(context.Background(), installPreflightReport{SchemaVersion: 1, Issues: []installPreflightIssue{}})
			if !slices.ContainsFunc(report.Issues, func(issue installPreflightIssue) bool { return issue.Title == test.title }) {
				t.Fatalf("missing %q: %+v", test.title, report)
			}
			if test.only && slices.ContainsFunc(report.Issues, func(issue installPreflightIssue) bool { return issue.Title != test.title }) {
				t.Fatalf("dependent inspections read through an unsafe path: %+v", report)
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.home)) {
				t.Fatal("the review changed files")
			}
			managedRequireNoServiceMutation(t, f.calls)
		})
	}
}

func holdInstallLock(t *testing.T, directory, name string) {
	t.Helper()
	lock, err := installLock(directory, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
}
