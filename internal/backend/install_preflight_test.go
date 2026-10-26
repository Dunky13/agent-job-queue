package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func preflightIDs(report installPreflightReport) map[string]bool {
	ids := map[string]bool{}
	for _, issue := range report.Issues {
		ids[issue.ID] = true
	}
	return ids
}

func installTreeWitness(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		value := info.Mode().String() + info.ModTime().String()
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			value += hex.EncodeToString(sum[:])
		}
		result[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestInstallerPreflightCollectsIndependentBlockersWithoutMutation(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	f.tasks = Object{"8": Object{"id": 8, "label": "Fixture job", "status": Object{"Paused": Object{}}}}
	integrationTestWrite(t, filepath.Join(f.installer.config, "remote.json"), []byte(`{"enabled":true}`))
	integrationTestWrite(t, filepath.Join(f.installer.state, "receipts/9.json"), []byte(`{"task_id":9,"token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
	before := installTreeWitness(t, f.installer.home)
	report := f.installer.preflight(context.Background())
	ids := preflightIDs(report)
	for _, id := range []string{"legacy-jobs", "pending-cleanup", "legacy-phone-access"} {
		if !ids[id] {
			t.Errorf("missing independent blocker %s: %+v", id, report)
		}
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.home)) {
		t.Fatal("read-only preflight changed installation files, modes or timestamps")
	}
	for _, args := range f.calls {
		if len(args) > 1 && (args[1] == "bootout" || args[1] == "bootstrap" || args[1] == "disable") {
			t.Fatalf("preflight mutated service: %v", args)
		}
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"schemaVersion":1`)) || !bytes.Contains(raw, []byte(`"destructive":true`)) || len(raw) > 32*1024 {
		t.Fatalf("unexpected wire report: %s", raw)
	}
}

func TestInstallerPreflightFreshInstallationCreatesNothing(t *testing.T) {
	f := installTestFixture(t)
	before := installTreeWitness(t, f.installer.home)
	report := f.installer.preflight(context.Background())
	if len(report.Issues) != 0 {
		t.Fatalf("fresh installation blocked: %+v", report)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.home)) {
		t.Fatal("fresh preflight created installation state")
	}
}

func TestInstallerPreflightInspectsLocksWithoutCreatingOrRemoving(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	filename := filepath.Join(f.installer.state, "install.lock")
	integrationTestWrite(t, filename, []byte("retained lock bytes"))
	fd, err := unix.Open(filename, unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	before := installTreeWitness(t, f.installer.home)
	report := f.installer.preflight(context.Background())
	if !preflightIDs(report)["installer-busy"] {
		t.Fatalf("held lock not reported: %+v", report)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.home)) {
		t.Fatal("lock inspection changed private files")
	}
}

func TestInstallerPreflightPrivateSymlinkAndInspectionErrorsFailClosed(t *testing.T) {
	for _, kind := range []string{"symlink", "query-error"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			if kind == "symlink" {
				target := filepath.Join(f.installer.home, "other")
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(f.installer.state, f.installer.state+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, f.installer.state); err != nil {
					t.Fatal(err)
				}
			} else {
				original := f.installer.command
				f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
					if args[0] == "launchctl" && args[1] == "print" {
						return nil, errors.New("inspection timeout")
					}
					return original(ctx, args)
				}
			}
			report := f.installer.preflight(context.Background())
			ids := preflightIDs(report)
			if kind == "symlink" && !ids["private-path-unsafe"] || kind == "query-error" && !ids["service-configuration"] {
				t.Fatalf("unsafe preflight passed: %+v", report)
			}
		})
	}
}

func TestInstallerRecoveryCleanupAndCancellationUseSelectedOwnedJob(t *testing.T) {
	for _, action := range []string{"cancel-job", "cleanup-job"} {
		t.Run(action, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			id := 7
			if action == "cleanup-job" {
				f.tasks = Object{"7": Object{"id": 7, "status": Object{"Done": Object{"result": "Success"}}}}
				integrationTestWrite(t, filepath.Join(f.installer.state, "receipts/7.json"), []byte(`{"task_id":7,"token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
			} else {
				f.tasks = Object{"7": Object{"id": 7, "status": Object{"Queued": Object{}}}}
				original := f.installer.command
				f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
					if args[0] == "pueue" && len(args) > 5 {
						switch args[5] {
						case "stash":
							f.tasks["7"].(Object)["status"] = Object{"Stashed": Object{}}
						case "remove":
							delete(f.tasks, "7")
						}
					}
					return original(ctx, args)
				}
			}
			if err := f.installer.recover(context.Background(), action, &id, ""); err != nil {
				t.Fatal(err)
			}
			if action == "cleanup-job" {
				if _, err := os.Stat(filepath.Join(f.installer.state, "receipts/7.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("verified completed receipt not cleaned")
				}
			} else if len(f.tasks) != 0 {
				t.Fatal("selected queued job not cancelled")
			}
		})
	}
}

func TestInstallerRecoveryRejectsStaleJobAndCustomCallback(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	id := 7
	f.tasks = Object{"7": Object{"id": 7, "status": Object{"Done": Object{"result": "Success"}}}}
	if err := f.installer.recover(context.Background(), "cancel-job", &id, ""); err == nil {
		t.Fatal("completed job was cancelled")
	}
	f.tasks["7"].(Object)["status"] = Object{"Running": Object{}}
	config := f.installer.freshConfig()
	config["daemon"].(Object)["callback"] = "custom-hook {{ id }}"
	integrationTestWrite(t, filepath.Join(f.installer.config, "pueue.yml"), mustMetadataJSON(t, config))
	if err := f.installer.recover(context.Background(), "cancel-job", &id, ""); err == nil || !strings.Contains(err.Error(), "custom callback") {
		t.Fatalf("custom hook recovery not rejected: %v", err)
	}
}

func TestInstallerCallbackRecoveryRequiresReviewedRevisionAndPreservesOriginal(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	config := f.installer.freshConfig()
	config["daemon"].(Object)["callback"] = "custom-hook {{ id }}"
	path := filepath.Join(f.installer.config, "pueue.yml")
	original := mustMetadataJSON(t, config)
	integrationTestWrite(t, path, original)
	report := f.installer.preflight(context.Background())
	if !preflightIDs(report)["custom-callback"] || len(report.Revision) != 64 {
		t.Fatalf("missing reviewed proposal: %+v", report)
	}
	if err := f.installer.recover(context.Background(), "reset-callback", nil, strings.Repeat("0", 64)); err == nil {
		t.Fatal("stale proposal replaced callback")
	}
	if !bytes.Equal(original, integrationTestRead(t, path)) {
		t.Fatal("stale proposal changed configuration")
	}
	if err := f.installer.recover(context.Background(), "reset-callback", nil, report.Revision); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(f.installer.config, "pueue.before-upgrade-"+report.Revision+".yml")
	if !bytes.Equal(original, integrationTestRead(t, backup)) {
		t.Fatal("original custom configuration was not preserved")
	}
	node, err := installConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	callback, err := installConfigCallback(node)
	if err != nil || !f.installer.standardCleanupCallback(callback) {
		t.Fatalf("reviewed callback repair invalid: %q %v", callback, err)
	}
}

func TestInstallerPreflightModernUpgradeRetainsActiveJobsAndOffersStoppedServiceStart(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	f.tasks = Object{"8": Object{"id": 8, "status": Object{"Running": Object{}}}}
	if report := f.installer.preflight(context.Background()); preflightIDs(report)["legacy-jobs"] {
		t.Fatal("modern upgrade requires legacy drain")
	}
	delete(f.loaded, "io.went.agent-heavy")
	report := f.installer.preflight(context.Background())
	if !preflightIDs(report)["service-not-ready"] {
		t.Fatalf("stopped service not diagnosed: %+v", report)
	}
	if err := f.installer.recover(context.Background(), "repair-service", nil, ""); err != nil {
		t.Fatal(err)
	}
	if !f.loaded["io.went.agent-heavy"] {
		t.Fatal("verified stopped service was not started")
	}
}

func TestInstallerRecoveryArgumentParserRejectsAmbiguity(t *testing.T) {
	for _, suffix := range [][]string{{"--preflight"}, {"--json"}, {"--recover", ""}, {"--recover", "unknown"}, {"--recover", "wait"}, {"--recover", "cancel-job"}, {"--recover", "repair-service", "--job-id", "1"}, {"--recover", "reset-callback"}, {"--preflight", "--json", "--disable-phone-access"}, {"--recover", "cancel-job", "--job-id", "1", "--job-id", "2"}} {
		if _, err := installParseOptions(append([]string{"--payload", "/payload"}, suffix...)); err == nil {
			t.Fatalf("ambiguous recovery accepted: %v", suffix)
		}
	}
	for _, suffix := range [][]string{{"--preflight", "--json"}, {"--recover", "cancel-job", "--job-id", "7"}, {"--recover", "repair-service"}, {"--recover", "reset-callback", "--expected-revision", strings.Repeat("a", 64)}, {"--disable-phone-access"}} {
		if _, err := installParseOptions(append([]string{"--payload", "/payload"}, suffix...)); err != nil {
			t.Fatalf("valid recovery rejected: %v", err)
		}
	}
}

func TestInstallerPreflightReportKeepsUniqueIDsAndBoundedUnicode(t *testing.T) {
	report := installPreflightReport{SchemaVersion: 1, Issues: []installPreflightIssue{}}
	for i := 0; i < 24; i++ {
		path := "/" + strings.Repeat("long-private-directory/", 40) + strconv.Itoa(i)
		index := report.add("private-path-unsafe", "Review ownership", strings.Repeat("例\x00", 1200), path, "reveal")
		if index < 0 {
			t.Fatal("blocker unexpectedly omitted")
		}
		report.Issues[index].Jobs = []installRecoveryJob{{ID: i, Label: strings.Repeat("例", 120), Status: "Paused"}}
	}
	report.compact()
	seen := map[string]bool{}
	for _, issue := range report.Issues {
		if seen[issue.ID] {
			t.Fatal("duplicate issue identity")
		}
		seen[issue.ID] = true
		if !utf8.ValidString(issue.Detail) || strings.ContainsRune(issue.Detail, 0) {
			t.Fatal("invalid display text")
		}
	}
	raw, err := json.Marshal(report)
	if err != nil || len(raw) > 32*1024 || len(report.Issues) != 24 {
		t.Fatalf("report not bounded without dropping blockers: %d %v", len(raw), err)
	}
	if index := report.add("legacy-jobs", "Jobs", "Detail", ""); index != -1 {
		t.Fatal("full report produced an invalid writable index")
	}
}

func TestInstallerModernUpgradeRejectsMismatchedLoadedServicesBeforeMutation(t *testing.T) {
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-remote"} {
		t.Run(label, func(t *testing.T) {
			f := installTestFixture(t)
			if err := f.run(t); err != nil {
				t.Fatal(err)
			}
			if label == "io.went.agent-remote" {
				f.loaded[label] = true
				integrationTestWrite(t, filepath.Join(f.installer.config, "remote.json"), []byte(`{"enabled":true}`))
			}
			before := installTreeWitness(t, f.installer.root)
			command := f.installer.command
			f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "launchctl" && args[1] == "print" && filepath.Base(args[2]) == label {
					return []byte("program = /usr/bin/unrelated\narguments = {\n/usr/bin/unrelated\nserve\n}\n"), nil
				}
				if args[0] == "launchctl" && (args[1] == "bootout" || args[1] == "kickstart") {
					t.Fatalf("unverified service mutation: %v", args)
				}
				return command(ctx, args)
			}
			if err := f.run(t); err == nil {
				t.Fatal("mismatched loaded service accepted")
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.root)) {
				t.Fatal("unverified service changed installed support")
			}
		})
	}
}

func TestInstallerPreflightFindsExternalProcessesAndStoppingController(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	delete(f.loaded, "io.went.agent-resources")
	filename := filepath.Join(f.installer.state, "resource-controller.lock")
	integrationTestWrite(t, filename, nil)
	fd, err := unix.Open(filename, unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	command := f.installer.command
	f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "ps" {
			return []byte("98123 " + filepath.Join(f.installer.root, ".venv/bin/python") + " " + filepath.Join(f.installer.root, "agent_heavy.py") + " scheduler"), nil
		}
		return command(ctx, args)
	}
	ids := preflightIDs(f.installer.preflight(context.Background()))
	if !ids["controller-stopping"] || !ids["legacy-processes"] {
		t.Fatalf("remaining controller/process not diagnosed: %v", ids)
	}
}

func TestInstallerPreflightSharedNativeWireFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "native", "tests", "fixtures", "bootstrap-preflight.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report installPreflightReport
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 1 || len(report.Issues) != 2 || len(report.Issues[0].Jobs) != 1 || report.Issues[0].Jobs[0].ID != 8 || len(report.Revision) != 64 {
		t.Fatalf("shared fixture shape changed: %+v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var expected, actual any
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("Go encoding changed the native contract:\n%s\n%s", raw, encoded)
	}
}

func TestInstallerRecoveryPhoneAccessPreservesLegacyAndGoCredentials(t *testing.T) {
	for _, kind := range []string{"legacy", "go"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			if kind == "legacy" {
				f.legacy(t)
			} else if err := f.run(t); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.installer.config, "remote.json")
			integrationTestWrite(t, path, []byte(`{"enabled":true,"password_hash":"retained hash","secret_key":"retained secret","port":8743}`))
			certificate := filepath.Join(f.installer.state, "remote/caddy/pki/authorities/local/root.crt")
			integrationTestWrite(t, certificate, []byte("retained CA"))
			calls := 0
			command := f.installer.command
			f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == filepath.Join(f.installer.payload, "source/bin/agent-remote") {
					calls++
					if len(args) != 2 || args[1] != "disable" {
						t.Fatalf("unexpected remote recovery command: %v", args)
					}
					settings, err := ReadPrivate(path)
					if err != nil {
						return nil, err
					}
					settings["enabled"] = false
					integrationTestWrite(t, path, mustMetadataJSON(t, settings))
					return nil, nil
				}
				return command(ctx, args)
			}
			if kind == "go" {
				if err := f.installer.recover(context.Background(), "disable-phone-access", nil, ""); err == nil || calls != 0 {
					t.Fatalf("healthy Go access could be disabled as a workaround: %v", err)
				}
				integrationTestWrite(t, filepath.Join(f.installer.state, "remote/error.json"), []byte(`{"error":"fixture remote migration failure"}`))
			}
			if err := f.installer.recover(context.Background(), "disable-phone-access", nil, ""); err != nil {
				t.Fatal(err)
			}
			settings, err := ReadPrivate(path)
			if err != nil || settings["enabled"] != false || settings["password_hash"] != "retained hash" || settings["secret_key"] != "retained secret" || calls != 1 || string(integrationTestRead(t, certificate)) != "retained CA" {
				t.Fatalf("remote recovery changed credentials or CA: %+v %v", settings, err)
			}
		})
	}
}
