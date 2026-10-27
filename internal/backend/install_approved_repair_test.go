package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInstallerApprovedRepairOptions(t *testing.T) {
	args := []string{"--payload", "/fixture/payload", "--repair-and-continue"}
	if _, err := installParseOptions(args); err != nil {
		t.Fatalf("approved automatic installation is unavailable: %v", err)
	}
	for _, extra := range [][]string{{"--preflight", "--json"}, {"--recover", "repair-service"}, {"--disable-phone-access"}, {"--repair-and-continue"}, {"--job-id", "1"}, {"--json"}} {
		if _, err := installParseOptions(append(append([]string{}, args...), extra...)); err == nil {
			t.Fatalf("ambiguous approval accepted: %v", extra)
		}
	}
}

func approvedTestIssue(id, action string, jobs ...installRecoveryJob) installPreflightIssue {
	return installPreflightIssue{ID: id, Title: id, Actions: []installRecoveryAction{{ID: action}}, Jobs: jobs}
}

func approvedTestReport(issues ...installPreflightIssue) installPreflightReport {
	return installPreflightReport{SchemaVersion: 1, Issues: issues}
}

func TestInstallerApprovedRepairKnownIssueContract(t *testing.T) {
	for _, test := range []struct {
		name  string
		issue installPreflightIssue
		want  bool
	}{
		{"restore", approvedTestIssue("service-definitions-missing", installRestoreServices), true},
		{"pending", approvedTestIssue("pending-installation", installFinishPending), true},
		{"pending-unavailable", approvedTestIssue("pending-installation", "reveal"), false},
		{"pending-foreign-action", approvedTestIssue("service-definitions-missing", installFinishPending), false},
		{"phone", approvedTestIssue("legacy-phone-access", "disable-phone-access"), true},
		{"remote-migration", approvedTestIssue("remote-migration", "disable-phone-access"), true},
		{"start", approvedTestIssue("service-not-ready", "repair-service"), true},
		{"service-wait", approvedTestIssue("service-not-ready", "wait"), true},
		{"cleanup", approvedTestIssue("pending-cleanup", "cleanup-job", installRecoveryJob{ID: 7, Status: "Cleanup pending"}), true},
		{"cleanup-wait", approvedTestIssue("pending-cleanup", "wait"), true},
		{"running", approvedTestIssue("legacy-jobs", "wait", installRecoveryJob{ID: 7, Status: "Running"}), true},
		{"queued", approvedTestIssue("legacy-jobs", "wait", installRecoveryJob{ID: 7, Status: "Queued"}), true},
		{"paused", approvedTestIssue("legacy-jobs", "wait", installRecoveryJob{ID: 7, Status: "Paused"}), false},
		{"stashed", approvedTestIssue("legacy-jobs", "wait", installRecoveryJob{ID: 7, Status: "Stashed"}), false},
		{"no-jobs", approvedTestIssue("legacy-jobs", "wait"), false},
		{"unknown", approvedTestIssue("unknown", "wait"), false},
		{"custom", approvedTestIssue("custom-callback", "reset-callback"), false},
		{"cancel", approvedTestIssue("legacy-jobs", "cancel-job", installRecoveryJob{ID: 7, Status: "Running"}), false},
		{"negative-job", approvedTestIssue("pending-cleanup", "cleanup-job", installRecoveryJob{ID: -1, Status: "Done"}), false},
		{"foreign-action", approvedTestIssue("service-definitions-missing", "reset-callback"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, ok := installApprovedIssue(test.issue); ok != test.want {
				t.Fatalf("approval eligibility %v; want %v", ok, test.want)
			}
		})
	}
}

func TestInstallerApprovedRepairSequencesThenInstalls(t *testing.T) {
	reports := []installPreflightReport{
		approvedTestReport(approvedTestIssue("service-definitions-missing", installRestoreServices)),
		approvedTestReport(approvedTestIssue("pending-cleanup", "cleanup-job", installRecoveryJob{ID: 7, Status: "Done"})),
		approvedTestReport(approvedTestIssue("legacy-phone-access", "disable-phone-access")),
		approvedTestReport(approvedTestIssue("legacy-jobs", "wait", installRecoveryJob{ID: 8, Status: "Running"})),
		approvedTestReport(),
	}
	stage, waits, installs := 0, 0, 0
	var actions []string
	err := installRunApprovedRepair(context.Background(), func(context.Context) installPreflightReport {
		return reports[stage]
	}, func(_ context.Context, action string, jobID *int, revision string) error {
		if revision != "" || (action == "cleanup-job") != (jobID != nil) || jobID != nil && *jobID != 7 {
			t.Fatal("repair changed its approved identity")
		}
		actions = append(actions, action)
		stage++
		return nil
	}, func(context.Context) error {
		installs++
		return nil
	}, func(context.Context) error {
		waits++
		if waits == 3 {
			stage++
		}
		return nil
	}, func(string) {})
	if err != nil || installs != 1 || waits != 3 || !reflect.DeepEqual(actions, []string{installRestoreServices, "cleanup-job", "disable-phone-access"}) {
		t.Fatalf("unexpected automatic setup actions=%v waits=%d installs=%d error=%v", actions, waits, installs, err)
	}
}

func TestInstallerApprovedRepairCoordinationWaitsBeforeMutation(t *testing.T) {
	for _, busy := range []string{"installer-busy", "scheduling-busy", "remote-busy", "controller-stopping"} {
		t.Run(busy, func(t *testing.T) {
			stage, waits := 0, 0
			err := installRunApprovedRepair(context.Background(), func(context.Context) installPreflightReport {
				if stage == 0 {
					return approvedTestReport(approvedTestIssue("service-definitions-missing", installRestoreServices), approvedTestIssue(busy, "wait"))
				}
				if stage == 1 {
					return approvedTestReport(approvedTestIssue("service-definitions-missing", installRestoreServices))
				}
				return approvedTestReport()
			}, func(context.Context, string, *int, string) error {
				if waits != 1 || stage != 1 {
					t.Fatal("mutated while coordination was busy")
				}
				stage++
				return nil
			}, func(context.Context) error { return nil }, func(context.Context) error {
				waits++
				stage++
				return nil
			}, func(string) {})
			if err != nil || stage != 2 {
				t.Fatalf("busy recovery failed safely: %v", err)
			}
		})
	}
}

func TestInstallerApprovedRepairStopsOnFailureUnknownOrNoProgress(t *testing.T) {
	for _, kind := range []string{"recovery-error", "unknown-next", "no-progress", "cancelled-wait", "malformed-schema"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mutations, installs := 0, 0
			wanted := errors.New("verified recovery failure")
			err := installRunApprovedRepair(ctx, func(context.Context) installPreflightReport {
				if kind == "malformed-schema" {
					return installPreflightReport{SchemaVersion: 2}
				}
				if kind == "cancelled-wait" {
					return approvedTestReport(approvedTestIssue("installer-busy", "wait"))
				}
				if kind == "unknown-next" && mutations > 0 {
					return approvedTestReport(approvedTestIssue("custom-callback", "reset-callback"))
				}
				return approvedTestReport(approvedTestIssue("service-definitions-missing", installRestoreServices))
			}, func(context.Context, string, *int, string) error {
				mutations++
				if kind == "recovery-error" {
					return wanted
				}
				return nil
			}, func(context.Context) error { installs++; return nil }, func(context.Context) error {
				cancel()
				return ctx.Err()
			}, func(string) {})
			if err == nil || mutations > 1 || installs != 0 {
				t.Fatalf("unsafe continuation mutations=%d installs=%d error=%v", mutations, installs, err)
			}
			if kind == "recovery-error" && !errors.Is(err, wanted) || kind == "cancelled-wait" && !errors.Is(err, context.Canceled) || kind == "no-progress" && !strings.Contains(err.Error(), "No repair was repeated") {
				t.Fatalf("original failure was not retained: %v", err)
			}
		})
	}
}

func TestInstallerApprovedRepairCancellationBeforeMutation(t *testing.T) {
	for _, kind := range []string{"inspect-install", "inspect-repair", "progress-install", "progress-repair"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mutations := 0
			err := installRunApprovedRepair(ctx, func(context.Context) installPreflightReport {
				if strings.HasPrefix(kind, "inspect-") {
					cancel()
				}
				if strings.HasSuffix(kind, "repair") {
					return approvedTestReport(approvedTestIssue("service-definitions-missing", installRestoreServices))
				}
				return approvedTestReport()
			}, func(context.Context, string, *int, string) error {
				mutations++
				return nil
			}, func(context.Context) error {
				mutations++
				return nil
			}, func(context.Context) error { return ctx.Err() }, func(string) {
				if strings.HasPrefix(kind, "progress-") {
					cancel()
				}
			})
			if !errors.Is(err, context.Canceled) || mutations != 0 {
				t.Fatalf("cancelled approval started a mutation: count=%d error=%v", mutations, err)
			}
		})
	}
}

func TestInstallerApprovedRepairRealMissingFilesAndDefaultConsent(t *testing.T) {
	f, _ := missingServiceFixture(t)
	install := f.installer
	f.tasks = Object{"7": Object{"id": 7, "status": Object{"Done": Object{"result": "Success"}}}}
	integrationTestWrite(t, filepath.Join(install.state, "receipts/7.json"), []byte(`{"task_id":7,"token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
	if err := os.Mkdir(filepath.Join(install.state, "remote"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := install.writePlist(filepath.Join(install.launch, "io.went.agent-remote.plist"), Object{
		"Label": "io.went.agent-remote", "ProgramArguments": []string{filepath.Join(install.root, "remote/.venv/bin/python"), filepath.Join(install.root, "remote/control.py"), "serve"},
	}); err != nil {
		t.Fatal(err)
	}
	f.loaded["io.went.agent-remote"] = true
	original := install.command
	phoneDisables := 0
	install.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 2 && args[0] == filepath.Join(install.payload, "source/bin/agent-remote") && args[1] == "disable" {
			phoneDisables++
			delete(f.loaded, "io.went.agent-remote")
			return nil, nil
		}
		return original(ctx, args)
	}
	if err := f.run(t); err == nil {
		t.Fatal("unapproved default installation repaired missing ownership")
	}
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources"} {
		if _, err := os.Lstat(filepath.Join(f.installer.launch, label+".plist")); !os.IsNotExist(err) {
			t.Fatal("default install created a service definition", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.installer.repairAndExecute(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.installer.embeddedDefinition() {
		t.Fatal("approved repair did not finish the embedded migration")
	}
	if _, err := os.Lstat(filepath.Join(f.installer.root, "install-in-progress.json")); !os.IsNotExist(err) {
		t.Fatal("approved install did not finish", err)
	}
	if phoneDisables != 1 {
		t.Fatalf("phone disable count=%d; want exactly one approved disable", phoneDisables)
	}
	if _, err := os.Lstat(filepath.Join(install.state, "receipts/7.json")); !os.IsNotExist(err) {
		t.Fatal("verified completed cleanup did not finish", err)
	}
}

func TestInstallerApprovedRepairPriorityAndLimit(t *testing.T) {
	issues := []installPreflightIssue{
		approvedTestIssue("legacy-phone-access", "disable-phone-access"),
		approvedTestIssue("pending-cleanup", "cleanup-job", installRecoveryJob{ID: 7, Status: "Done"}),
		approvedTestIssue("service-not-ready", "repair-service"),
		approvedTestIssue("service-definitions-missing", installRestoreServices),
	}
	for _, expected := range []string{installRestoreServices, "repair-service", "cleanup-job", "disable-phone-access"} {
		step, err := installApprovedNext(approvedTestReport(issues...))
		if err != nil || step.action != expected {
			t.Fatalf("priority=%s, want=%s, error=%v", step.action, expected, err)
		}
		issues = issues[:len(issues)-1]
	}
	mutations := 0
	err := installRunApprovedRepair(context.Background(), func(context.Context) installPreflightReport {
		return approvedTestReport(approvedTestIssue("pending-cleanup", "cleanup-job", installRecoveryJob{ID: mutations, Status: "Done"}))
	}, func(context.Context, string, *int, string) error { mutations++; return nil }, func(context.Context) error {
		t.Fatal("unbounded repairs authorized installation")
		return nil
	}, func(context.Context) error { t.Fatal("unexpected wait"); return nil }, func(string) {})
	if err == nil || mutations != 128 || !strings.Contains(err.Error(), "repair limit") {
		t.Fatalf("unbounded repair work: mutations=%d error=%v", mutations, err)
	}
}
