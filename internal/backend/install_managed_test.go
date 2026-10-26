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

	"github.com/Dunky13/agent-job-queue/internal/appservices"
)

type managedFixture struct {
	*installFixture
	statuses      map[string]string
	approval      string
	nativeFailure string
	badStatus     bool
}

func managedTestFixture(t *testing.T) *managedFixture {
	t.Helper()
	f := installTestFixture(t)
	app := filepath.Join(f.installer.payload, "Agent Queue.app")
	info, err := updatePlist(filepath.Join(app, "Contents/Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	info["AgentQueueServiceManagement"] = true
	if err := f.installer.writePlist(filepath.Join(app, "Contents/Info.plist"), info); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		integrationTestWrite(t, filepath.Join(app, "Contents/MacOS", name), []byte("#!/bin/sh\nexit 0\n"))
		if err := os.Chmod(filepath.Join(app, "Contents/MacOS", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for service, command := range map[string]string{"heavy": "daemon", "resources": "scheduler", "remote": "serve"} {
		helper := "agent-heavy"
		if service == "remote" {
			helper = "agent-remote"
		}
		filename := filepath.Join(app, "Contents/Library/LaunchAgents/io.went.agent-"+service+".plist")
		integrationTestWrite(t, filename, []byte("placeholder"))
		if err := f.installer.writePlist(filename, Object{"Label": "io.went.agent-" + service, "BundleProgram": "Contents/MacOS/" + helper, "ProgramArguments": []string{helper, command}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := installCopyTree(filepath.Join(f.installer.payload, "source"), filepath.Join(app, "Contents/Resources/payload/source")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(app, "Contents/Resources/payload/source/bin")); err != nil {
		t.Fatal(err)
	}
	m := &managedFixture{installFixture: f, statuses: map[string]string{"queue": "notRegistered", "resources": "notRegistered", "remote": "notRegistered", "login": "notRegistered"}}
	original := f.installer.command
	f.installer.queueSnapshot = func(context.Context) (Object, error) { return f.snapshot(), nil }
	f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[1] == "--prepare-app-replacement" {
			f.calls = append(f.calls, slices.Clone(args))
			return mustMetadataJSON(t, Object{"schemaVersion": 1, "app": args[2], "status": "stopped"}), nil
		}
		if len(args) == 4 && args[1] == "--service-management" {
			f.calls = append(f.calls, slices.Clone(args))
			action, service := args[2], args[3]
			if m.nativeFailure == action+" "+service {
				return nil, errors.New("simulated native failure")
			}
			if action == "status" {
				if m.badStatus {
					return []byte(`{"schemaVersion":1,"service":"queue","status":"enabled","unexpected":true}`), nil
				}
				return mustMetadataJSON(t, appservices.Status{SchemaVersion: 1, Service: service, Status: m.statuses[service]}), nil
			}
			label := map[string]string{"queue": "io.went.agent-heavy", "resources": "io.went.agent-resources", "remote": "io.went.agent-remote"}[service]
			if action == "register" {
				if _, err := os.Stat(filepath.Join(f.installer.app, "Contents/MacOS/agent-heavy")); err != nil {
					t.Fatal("registration preceded complete bundle publication", err)
				}
				if _, err := os.Stat(filepath.Join(f.installer.root, "install-in-progress.json")); err != nil {
					t.Fatal("registration lost pending marker", err)
				}
				m.statuses[service] = "enabled"
				if m.approval == service {
					m.statuses[service] = "requiresApproval"
				}
				if label != "" && m.statuses[service] == "enabled" {
					f.loaded[label] = true
				}
			} else if action == "unregister" {
				m.statuses[service] = "notRegistered"
				delete(f.loaded, label)
			}
			return nil, nil
		}
		if len(args) > 2 && args[0] == "launchctl" && args[1] == "print" {
			label := filepath.Base(args[2])
			service := map[string]string{"io.went.agent-heavy": "queue", "io.went.agent-resources": "resources", "io.went.agent-remote": "remote"}[label]
			if service != "" && m.statuses[service] == "enabled" && f.loaded[label] {
				f.calls = append(f.calls, slices.Clone(args))
				helper, command := "agent-heavy", "daemon"
				if service == "resources" {
					command = "scheduler"
				}
				if service == "remote" {
					helper, command = "agent-remote", "serve"
				}
				executable := filepath.Join(f.installer.app, "Contents/MacOS", helper)
				return []byte("program = " + executable + "\narguments = {\n" + helper + "\n" + command + "\n}\n"), nil
			}
		}
		if len(args) == 3 && args[0] == filepath.Join(f.installer.app, "Contents/MacOS/agent-heavy") && args[1] == "status" {
			f.calls = append(f.calls, slices.Clone(args))
			return mustMetadataJSON(t, Object{"updatedAt": time.Now().UTC().Format(time.RFC3339Nano), "queue": Object{"status": "running", "limit": 1, "running": 0, "waiting": 0}, "jobs": []any{}, "scheduler": Object{}}), nil
		}
		return original(ctx, args)
	}
	return m
}

func TestManagedFreshInstallUsesOnlyBundleHelpers(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	for _, path := range []string{i.launch, filepath.Join(i.root, "bin"), filepath.Join(i.root, "install-in-progress.json")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected legacy or pending path %s: %v", path, err)
		}
	}
	metadata, err := appservices.Load(i.home)
	if err != nil || metadata == nil || metadata.App != i.app {
		t.Fatalf("metadata %v: %v", metadata, err)
	}
	app, err := updateRegisteredApp()
	if err != nil || app != i.app {
		t.Fatalf("updater location %q: %v", app, err)
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		raw, err := os.ReadFile(filepath.Join(i.home, ".local/bin", name))
		if err != nil || !strings.Contains(string(raw), installQuote(filepath.Join(i.app, "Contents/MacOS", name))) || strings.Contains(string(raw), "share/agent-heavy/bin") {
			t.Fatalf("incorrect wrapper: %s %v", raw, err)
		}
	}
	for _, args := range m.calls {
		if args[0] == "launchctl" && args[1] != "print" {
			t.Fatalf("fresh install used user service mutation %v", args)
		}
	}
}

func TestManagedFinderBundleAndPreflightAreReadOnly(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
		t.Fatal(err)
	}
	i.appBundle = true
	i.payload = filepath.Join(i.app, "Contents/Resources/payload")
	before := installTreeWitness(t, i.home)
	report := i.preflight(context.Background())
	if len(report.Issues) != 0 {
		t.Fatalf("fresh preflight blockers: %+v", report.Issues)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("preflight modified home")
	}
	for _, args := range m.calls {
		if len(args) > 2 && args[1] == "--service-management" && args[2] != "status" {
			t.Fatal("preflight mutated service", args)
		}
	}
	bundle := installBundleSnapshot(t, i.app)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bundle, installBundleSnapshot(t, i.app)) {
		t.Fatal("Finder-installed bundle was modified")
	}
}

func TestManagedApprovalFailureRetainsPendingAndNeverReregisters(t *testing.T) {
	m := managedTestFixture(t)
	m.approval = "resources"
	if err := m.run(t); err == nil {
		t.Fatal("approval refusal completed install")
	}
	i := m.installer
	if _, err := os.Stat(filepath.Join(i.root, "install-in-progress.json")); err != nil {
		t.Fatal("lost partial-failure marker", err)
	}
	if _, err := updateRegisteredApp(); err == nil {
		t.Fatal("updater ignored pending installation")
	}
	before := installTreeWitness(t, i.home)
	m.calls = nil
	if err := m.run(t); err == nil {
		t.Fatal("approval-required retry completed")
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("denied approval retry changed installation")
	}
	for _, args := range m.calls {
		if len(args) > 2 && args[1] == "--service-management" && args[2] != "status" {
			t.Fatal("re-registered approval-required service", args)
		}
	}
	m.approval = ""
	m.statuses["resources"] = "enabled"
	m.loaded["io.went.agent-resources"] = true
	if err := m.run(t); err != nil {
		t.Fatal("approved retry failed", err)
	}
}

func TestManagedDrainGatePrecedesEveryMutation(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "managed"}[managed], func(t *testing.T) {
			m := managedTestFixture(t)
			if managed {
				if err := m.run(t); err != nil {
					t.Fatal(err)
				}
			} else {
				i := m.installer
				for _, directory := range []string{i.state, i.launch} {
					if err := os.MkdirAll(directory, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := i.writePlist(filepath.Join(i.launch, "io.went.agent-heavy.plist"), Object{"Label": "io.went.agent-heavy", "ProgramArguments": []string{filepath.Join(i.root, "bin/agent-heavy"), "daemon"}}); err != nil {
					t.Fatal(err)
				}
				m.loaded["io.went.agent-heavy"] = true
			}
			m.tasks["41"] = Object{"status": Object{"Running": Object{}}}
			before := installTreeWitness(t, m.installer.home)
			m.calls = nil
			if err := m.run(t); err == nil || !strings.Contains(err.Error(), "drained") {
				t.Fatal("missing drain refusal", err)
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, m.installer.home)) {
				t.Fatal("drain refusal mutated installation")
			}
			for _, args := range m.calls {
				if args[0] == "launchctl" && args[1] != "print" || len(args) > 2 && args[1] == "--service-management" && args[2] != "status" {
					t.Fatal("drain refusal mutated services", args)
				}
			}
		})
	}
}

func TestManagedLegacyGoMigrationAndRetry(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	// Generate the canonical legacy definitions without installing any services.
	for _, directory := range []string{i.launch, i.state, filepath.Join(i.root, "bin")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := i.validateServices(); err != nil {
		t.Fatal(err)
	}
	if err := i.serviceDefinitions(false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		integrationTestWrite(t, filepath.Join(i.root, "bin", name), []byte("old helper"))
	}
	m.loaded["io.went.agent-heavy"] = true
	m.loaded["io.went.agent-resources"] = true
	m.loaded["io.went.agent-queue"] = true
	m.nativeFailure = "register resources"
	if err := m.run(t); err == nil {
		t.Fatal("missing native failure")
	}
	if _, err := os.Stat(filepath.Join(i.root, "install-in-progress.json")); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"heavy", "resources", "queue"} {
		if _, err := os.Lstat(filepath.Join(i.launch, "io.went.agent-"+label+".plist")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("retained legacy definition", label, err)
		}
	}
	m.nativeFailure = ""
	if err := m.run(t); err != nil {
		t.Fatal("safe retry failed", err)
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		if _, err := os.Lstat(filepath.Join(i.root, "bin", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("retained hidden helper", err)
		}
	}
}

func TestManagedRefusesUnownedAndCustomizedLegacyServices(t *testing.T) {
	for _, kind := range []string{"missing-definition", "custom-env", "custom-option", "wrong-loaded-executable"} {
		t.Run(kind, func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			if err := os.MkdirAll(i.launch, 0700); err != nil {
				t.Fatal(err)
			}
			definition := Object{"Label": "io.went.agent-heavy", "ProgramArguments": []string{filepath.Join(i.root, "bin/agent-heavy"), "daemon"}}
			if kind == "custom-env" {
				definition["EnvironmentVariables"] = Object{"AGENT_HEAVY_TEST_MODE": "1"}
			}
			if kind == "custom-option" {
				definition["ProcessType"] = "Interactive"
			}
			if kind != "missing-definition" {
				if err := i.writePlist(filepath.Join(i.launch, "io.went.agent-heavy.plist"), definition); err != nil {
					t.Fatal(err)
				}
			}
			m.loaded["io.went.agent-heavy"] = true
			if kind == "wrong-loaded-executable" {
				original := i.command
				i.command = func(ctx context.Context, args []string) ([]byte, error) {
					if slices.Equal(args, []string{"launchctl", "print", i.domain() + "/io.went.agent-heavy"}) {
						return []byte("program = /unknown\narguments = {\n/unknown\n}\n"), nil
					}
					return original(ctx, args)
				}
			}
			before := installTreeWitness(t, i.home)
			if err := m.run(t); err == nil {
				t.Fatal("unsafe migration accepted")
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("ownership refusal modified installation")
			}
		})
	}
}

func TestManagedInvalidNativeJSONAndInstallationMetadata(t *testing.T) {
	for _, kind := range []string{"json", "metadata", "marker", "signature"} {
		t.Run(kind, func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "json":
				m.badStatus = true
			case "metadata":
				integrationTestWrite(t, filepath.Join(i.root, "installation.json"), []byte(`{"schemaVersion":99,"app":"/unknown","serviceManagement":true}`))
			case "marker":
				info, err := updatePlist(filepath.Join(i.payload, "Agent Queue.app/Contents/Info.plist"))
				if err != nil {
					t.Fatal(err)
				}
				info["AgentQueueServiceManagement"] = "true"
				if err := i.writePlist(filepath.Join(i.payload, "Agent Queue.app/Contents/Info.plist"), info); err != nil {
					t.Fatal(err)
				}
			case "signature":
				m.fail = "codesign --verify"
			}
			before := installTreeWitness(t, i.home)
			if err := m.run(t); err == nil {
				t.Fatal("invalid managed install accepted")
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("invalid install mutated home")
			}
		})
	}
}

func TestManagedReplacementRetainsPreviousUntilSuccessfulRetry(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	previousDigest, err := managedBundleDigest(i.app)
	if err != nil {
		t.Fatal(err)
	}
	// Different signed bytes with the same development identity still require a
	// drained service restart; signature verification is mocked by this fixture.
	integrationTestWrite(t, filepath.Join(i.payload, "Agent Queue.app/Contents/Resources/new-resource"), []byte("new bundle bytes"))
	m.nativeFailure = "register resources"
	if err := m.run(t); err == nil {
		t.Fatal("missing partial replacement failure")
	}
	backup := filepath.Join(filepath.Dir(i.app), ".Agent Queue.previous.app")
	digest, err := managedBundleDigest(backup)
	if err != nil || digest != previousDigest {
		t.Fatal("known-good bundle lost", err)
	}
	m.nativeFailure = ""
	if err := m.run(t); err != nil {
		t.Fatal("replacement retry", err)
	}
	if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed retry did not retire backup", err)
	}
}

func TestManagedOfflineDrainReadsPrivateCopy(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := os.MkdirAll(i.state, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openQueueStore(i.state, true)
	if err != nil {
		t.Fatal(err)
	}
	store.close()
	before := installTreeWitness(t, i.home)
	if err := i.managedOfflineDrain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("offline drain changed source database")
	}
	store, err = openQueueStore(i.state, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(41,'fixture','{}','running')")
	store.close()
	if err != nil {
		t.Fatal(err)
	}
	if err := i.managedOfflineDrain(context.Background()); err == nil {
		t.Fatal("offline unfinished attempt accepted")
	}
}

func TestManagedUnregistrationMustCompleteBeforeReplacement(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	m.nativeFailure = "unregister queue"
	before := installBundleSnapshot(t, m.installer.app)
	if err := m.run(t); err == nil {
		t.Fatal("unregistration failure accepted")
	}
	if !reflect.DeepEqual(before, installBundleSnapshot(t, m.installer.app)) {
		t.Fatal("app replaced before unregister completion")
	}
}

func TestManagedInterruptedPublicationRestoresKnownGoodThenRetries(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(filepath.Join(i.root, "build-info.json"))
	if err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(i.root, "install-in-progress.json"), old)
	for _, service := range []string{"queue", "resources", "login"} {
		m.statuses[service] = "notRegistered"
	}
	m.loaded = map[string]bool{}
	backup := filepath.Join(filepath.Dir(i.app), ".Agent Queue.previous.app")
	identity, err := managedCaptureBundle(i.app)
	if err != nil {
		t.Fatal(err)
	}
	receipt := managedPublicationReceipt{SchemaVersion: 1, App: i.app, Pending: identity.Build, Candidate: identity, Backup: &identity}
	integrationTestWrite(t, i.managedReceiptPath(), mustMetadataJSON(t, receipt))
	if err := os.Rename(i.app, backup); err != nil {
		t.Fatal(err)
	}
	if err := m.run(t); err != nil {
		t.Fatal("interrupted publication could not recover", err)
	}
	if _, err := os.Stat(filepath.Join(i.app, "Contents/MacOS/AgentQueue")); err != nil {
		t.Fatal(err)
	}
}

func TestManagedPendingIdentityRefusesDowngrade(t *testing.T) {
	m := managedTestFixture(t)
	m.nativeFailure = "register resources"
	if err := m.run(t); err == nil {
		t.Fatal("missing pending install")
	}
	i := m.installer
	newer := metadataTestInfo(t, "124")
	integrationTestWrite(t, filepath.Join(i.root, "install-in-progress.json"), mustMetadataJSON(t, newer))
	before := installTreeWitness(t, i.home)
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatal("pending identity did not block downgrade", err)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("downgrade changed installation")
	}
}

func TestManagedReadinessRejectsWrongLoadedHelper(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if m.statuses["queue"] == "enabled" && slices.Equal(args, []string{"launchctl", "print", i.domain() + "/io.went.agent-heavy"}) {
			return []byte("program = /unknown\narguments = {\n/unknown\n}\n"), nil
		}
		return original(ctx, args)
	}
	if err := m.run(t); err == nil {
		t.Fatal("foreign readiness accepted")
	}
	if _, err := os.Stat(filepath.Join(i.root, "install-in-progress.json")); err != nil {
		t.Fatal("lost pending marker", err)
	}
}

func TestManagedCustomFinderLocationIsReadBackByUpdater(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	i.app = filepath.Join(i.home, "Applications/Finder Copied Queue.app")
	if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
		t.Fatal(err)
	}
	i.payload = filepath.Join(i.app, "Contents/Resources/payload")
	i.appBundle = true
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	app, err := updateRegisteredApp()
	if err != nil || app != i.app {
		t.Fatal("updater lost explicit app location", app, err)
	}
	version, err := updateInstalled(i.root, "")
	if err != nil || version["build"] != 123 {
		t.Fatal(version, err)
	}
}

func TestManagedPueuePythonMigrationPreservesImportAndRetiresRuntime(t *testing.T) {
	m := managedTestFixture(t)
	m.legacy(t)
	i := m.installer
	filename := filepath.Join(i.launch, "io.went.agent-resources.plist")
	definition, err := updatePlist(filename)
	if err != nil {
		t.Fatal(err)
	}
	delete(definition, "EnvironmentVariables") // Only the fixture's custom setting is unsupported.
	if err := i.writePlist(filename, definition); err != nil {
		t.Fatal(err)
	}
	m.tasks["41"] = Object{"id": 41, "status": Object{"Done": Object{"result": "Success"}}}
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(i.state, "legacy-queue-import.json")); err != nil {
		t.Fatal("legacy import lost", err)
	}
	for _, name := range []string{"agent_heavy.py", ".venv"} {
		if _, err := os.Lstat(filepath.Join(i.root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("legacy runtime retained", name, err)
		}
	}
	node, err := installConfig(filepath.Join(i.config, "pueue.yml"))
	if err != nil {
		t.Fatal(err)
	}
	callback, err := installConfigCallback(node)
	if err != nil || callback != installQuote(filepath.Join(i.home, ".local/bin/agent-heavy"))+" cleanup {{ id }}" {
		t.Fatal(callback, err)
	}
}

func TestManagedExplicitPhoneRecoveryStillRequiresDrainedQueue(t *testing.T) {
	m := managedTestFixture(t)
	m.legacy(t)
	i := m.installer
	i.disablePhoneAccess = true
	definition, err := updatePlist(filepath.Join(i.launch, "io.went.agent-resources.plist"))
	if err != nil {
		t.Fatal(err)
	}
	delete(definition, "EnvironmentVariables")
	if err := i.writePlist(filepath.Join(i.launch, "io.went.agent-resources.plist"), definition); err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(i.config, "remote.json"), []byte(`{"enabled":true}`))
	m.tasks["41"] = Object{"id": 41, "status": Object{"Running": Object{}}}
	before := installTreeWitness(t, i.home)
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "drained") {
		t.Fatal("phone recovery bypassed drain", err)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("phone recovery mutated active installation")
	}
	for _, args := range m.calls {
		if len(args) > 1 && args[1] == "disable" {
			t.Fatal("phone recovery ran before drain", args)
		}
	}
}

func TestManagedRejectsEmbeddedExecutableCopiesAndWrongStaticArguments(t *testing.T) {
	for _, kind := range []string{"bin", "arguments", "helper-symlink", "support-identity"} {
		t.Run(kind, func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			app := filepath.Join(i.payload, "Agent Queue.app")
			switch kind {
			case "bin":
				integrationTestWrite(t, filepath.Join(app, "Contents/Resources/payload/source/bin/agent-heavy"), []byte("duplicate"))
			case "arguments":
				filename := filepath.Join(app, "Contents/Library/LaunchAgents/io.went.agent-heavy.plist")
				value, err := updatePlist(filename)
				if err != nil {
					t.Fatal(err)
				}
				value["ProgramArguments"] = []string{"agent-heavy", "run", "unreviewed-command"}
				if err := i.writePlist(filename, value); err != nil {
					t.Fatal(err)
				}
			case "helper-symlink":
				helper := filepath.Join(app, "Contents/MacOS/agent-heavy")
				if err := os.Remove(helper); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(i.payload, "source/bin/agent-heavy"), helper); err != nil {
					t.Fatal(err)
				}
			case "support-identity":
				integrationTestWrite(t, filepath.Join(app, "Contents/Resources/payload/source/build-info.json"), mustMetadataJSON(t, metadataTestInfo(t, "124")))
			}
			if err := m.run(t); err == nil {
				t.Fatal("invalid managed layout accepted", kind)
			}
			if _, err := os.Lstat(i.root); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid package started installation", err)
			}
		})
	}
}

func TestManagedRetainedLegacyStateRequiresConfiguration(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	integrationTestWrite(t, filepath.Join(i.state, "pueue/state.json"), mustMetadataJSON(t, m.snapshot()))
	before := installTreeWitness(t, i.home)
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Fatal("legacy custody gate missing", err)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("legacy custody refusal modified state")
	}
}

func TestManagedLegacyOwnershipIsRecheckedImmediatelyBeforeShutdown(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	for _, directory := range []string{i.launch, i.state} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := i.writePlist(filepath.Join(i.launch, "io.went.agent-heavy.plist"), Object{"Label": "io.went.agent-heavy", "ProgramArguments": []string{filepath.Join(i.root, "bin/agent-heavy"), "daemon"}}); err != nil {
		t.Fatal(err)
	}
	m.loaded["io.went.agent-heavy"] = true
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if slices.Equal(args, []string{"launchctl", "print", i.domain() + "/io.went.agent-heavy"}) {
			if _, err := os.Stat(filepath.Join(i.root, "install-in-progress.json")); err == nil {
				return []byte("program = /unknown\narguments = {\n/unknown\n}\n"), nil
			}
		}
		return original(ctx, args)
	}
	if err := m.run(t); err == nil {
		t.Fatal("changed loaded ownership accepted")
	}
	for _, args := range m.calls {
		if len(args) > 1 && args[0] == "launchctl" && args[1] == "bootout" {
			t.Fatal("stopped changed process", args)
		}
	}
}

func TestManagedFinderTargetMigratesOldCanonicalMenuLocation(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := os.MkdirAll(i.launch, 0700); err != nil {
		t.Fatal(err)
	}
	if err := i.validateServices(); err != nil {
		t.Fatal(err)
	}
	if err := i.serviceDefinitions(false); err != nil {
		t.Fatal(err)
	}
	i.app = filepath.Join(i.home, "Applications/Finder Copied Queue.app")
	if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
		t.Fatal(err)
	}
	i.payload = filepath.Join(i.app, "Contents/Resources/payload")
	i.appBundle = true
	if err := m.run(t); err != nil {
		t.Fatal("explicit Finder target failed to adopt canonical old menu", err)
	}
	if _, err := os.Lstat(filepath.Join(i.launch, "io.went.agent-queue.plist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old menu was retained", err)
	}
}
