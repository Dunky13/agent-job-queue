package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// managedCandidate replaces the bundle at the app path with build, as dragging a
// newer download over it would.
func managedCandidate(t *testing.T, m *managedFixture, build string) BuildInfo {
	t.Helper()
	return managedBundleBuild(t, m.installer, m.installer.app, build)
}

func managedBundleBuild(t *testing.T, i *installer, app, build string) BuildInfo {
	t.Helper()
	identity := metadataTestInfo(t, build)
	for _, filename := range []string{filepath.Join(app, "Contents/Resources/payload/source/build-info.json"), filepath.Join(app, "Contents/Resources/build-info.json")} {
		integrationTestWrite(t, filename, mustMetadataJSON(t, identity))
	}
	info, err := updatePlist(filepath.Join(app, "Contents/Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	info["CFBundleVersion"] = build
	info["CFBundleShortVersionString"] = identity.Version
	if err := i.writePlist(filepath.Join(app, "Contents/Info.plist"), info); err != nil {
		t.Fatal(err)
	}
	return identity
}

// managedOlderRegistered completes the installation an older release left: its
// helpers registered with Service Management, and running under launchd. This
// build never registers them on its own, but resume still has to.
func managedOlderRegistered(ctx context.Context, m *managedFixture) error {
	i := m.installer
	identity, err := i.validateBundle()
	if err != nil {
		return err
	}
	i.identity = identity
	for _, directory := range []string{i.root, i.state, i.config, filepath.Join(i.home, ".local/bin"), filepath.Join(i.state, "remote")} {
		if err := installDirectory(directory); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	marker := filepath.Join(i.root, "install-in-progress.json")
	if err := installAtomic(marker, append(raw, '\n'), 0600); err != nil {
		return err
	}
	if err := i.managedSupport(); err != nil {
		return err
	}
	metadata, err := json.Marshal(appservices.Installation{SchemaVersion: 1, App: i.app, ServiceManagement: true})
	if err != nil {
		return err
	}
	if err := installAtomic(filepath.Join(i.root, "installation.json"), append(metadata, '\n'), 0600); err != nil {
		return err
	}
	published, err := managedCaptureBundle(i.app)
	if err != nil {
		return err
	}
	if _, err := i.managedActivate(ctx, published, false); err != nil {
		return err
	}
	if err := installAtomic(filepath.Join(i.root, "build-info.json"), append(raw, '\n'), 0600); err != nil {
		return err
	}
	return os.Remove(marker)
}

func managedOlderRegistration(t *testing.T) *managedFixture {
	t.Helper()
	m := managedTestFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := managedOlderRegistered(ctx, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// managedOlderPublication leaves what an older release's installer left when it
// stopped right after publishing build over the bundle at the app path: the
// candidate there, the previous bundle retained beside it, the receipt binding
// both, and the pending marker. This build never publishes a copy itself.
func managedOlderPublication(t *testing.T, m *managedFixture, build string) (backup, candidate managedBundleIdentity) {
	t.Helper()
	i := m.installer
	if err := os.Rename(i.app, i.managedPreviousPath()); err != nil {
		t.Fatal(err)
	}
	if err := installCopyTree(i.managedPreviousPath(), i.app); err != nil {
		t.Fatal(err)
	}
	identity := managedBundleBuild(t, i, i.app, build)
	var err error
	if backup, err = managedCaptureBundle(i.managedPreviousPath()); err != nil {
		t.Fatal(err)
	}
	if candidate, err = managedCaptureBundle(i.app); err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, i.managedReceiptPath(), mustMetadataJSON(t, managedPublicationReceipt{SchemaVersion: 1, App: i.app, Pending: identity, Candidate: candidate, Backup: &backup}))
	integrationTestWrite(t, filepath.Join(i.root, "install-in-progress.json"), mustMetadataJSON(t, identity))
	m.seededPending = true
	return backup, candidate
}

func managedRemovalTarget(i *installer) {
	i.unregisterServices = true
}

func managedRequireNoServiceMutation(t *testing.T, calls [][]string) {
	t.Helper()
	for _, args := range calls {
		if len(args) > 2 && args[1] == "--service-management" && args[2] != "status" || len(args) > 1 && args[0] == "launchctl" && args[1] != "print" {
			t.Fatal("unexpected service mutation", args)
		}
	}
}

func TestManagedAbsentLegacyMenuRequiresExactLaunchctlEvidence(t *testing.T) {
	label := "io.went.agent-queue"
	output := fmt.Sprintf("Bad request.\nCould not find service %q in domain for user gui: %d\n", label, os.Getuid())
	if !installServiceAbsent([]byte(output), 113, label) {
		t.Fatal("canonical absent legacy menu was not recognized")
	}
	for _, witness := range []struct {
		output string
		code   int
		label  string
	}{
		{output, 1, label},
		{output + "unexpected failure", 113, label},
		{strings.Replace(output, "gui:", "system:", 1), 113, label},
		{output, 113, "foreign.service"},
		{strings.ReplaceAll(output, label, "foreign.service"), 113, "foreign.service"},
	} {
		if installServiceAbsent([]byte(witness.output), witness.code, witness.label) {
			t.Fatal("ambiguous service absence accepted", witness)
		}
	}
}

func TestManagedLifecycleOptionParsing(t *testing.T) {
	args := []string{"--app-bundle", "/Applications/Agent Queue.app", "--unregister-services"}
	options, err := installParseOptions(args)
	if err != nil || !options.unregisterServices || options.preflight || options.recovery != "" {
		t.Fatalf("valid lifecycle request %v: %+v %v", args, options, err)
	}
	for _, extra := range [][]string{{"--preflight", "--json"}, {"--preflight"}, {"--json"}, {"--recover", "disable-phone-access"}, {"--disable-phone-access"}, {"--repair-and-continue"}, {"--unregister-services"}} {
		if _, err := installParseOptions(append(slices.Clone(args), extra...)); err == nil {
			t.Fatal("invalid lifecycle combination accepted", args, extra)
		}
	}
	for _, args := range [][]string{
		{"--app-bundle", "/Applications/Agent Queue.app", "--prepare-replacement"},
		{"--payload", "/private/payload"},
		{"--payload", "/private/payload", "--prepare-replacement"},
		{"--payload", "/private/payload", "--unregister-services"},
		{"--app-bundle", "relative.app", "--unregister-services"},
		{"--app-bundle", "/private/Agent Queue.app", "--unregister-services"},
		{"--app-bundle", "/Applications/not-an-app", "--unregister-services"},
	} {
		if _, err := installParseOptions(args); err == nil {
			t.Fatal("invalid target accepted", args)
		}
	}
}

func TestManagedLifecycleRejectsUnmarkedLegacyCandidates(t *testing.T) {
	for _, removal := range []bool{false, true} {
		t.Run(map[bool]string{false: "setup", true: "remove"}[removal], func(t *testing.T) {
			f := installTestFixture(t)
			i := f.installer
			info, err := updatePlist(filepath.Join(i.app, "Contents/Info.plist"))
			if err != nil {
				t.Fatal(err)
			}
			delete(info, "AgentQueueServiceManagement")
			if err := i.writePlist(filepath.Join(i.app, "Contents/Info.plist"), info); err != nil {
				t.Fatal(err)
			}
			if removal {
				managedRemovalTarget(i)
			}
			before := installTreeWitness(t, i.home)
			if err := f.run(t); err == nil || !strings.Contains(err.Error(), "marker") {
				t.Fatal("unmarked lifecycle request accepted", err)
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("unmarked lifecycle request changed home")
			}
			managedRequireNoServiceMutation(t, f.calls)
		})
	}
}

func TestManagedLifecycleDrainRefusalChangesNothing(t *testing.T) {
	m := managedOlderRegistration(t)
	i := m.installer
	managedRemovalTarget(i)
	m.tasks["41"] = Object{"id": 41, "status": Object{"Running": Object{}}}
	before := installTreeWitness(t, i.home)
	m.calls = nil
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "drained") {
		t.Fatal("missing active-job refusal", err)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("active-job refusal modified home")
	}
	managedRequireNoServiceMutation(t, m.calls)
}

func TestManagedRemovalHoldsLocksThroughShutdown(t *testing.T) {
	m := managedOlderRegistration(t)
	i := m.installer
	originalBundle := installBundleSnapshot(t, i.app)
	identity, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil {
		t.Fatal(err)
	}
	managedRemovalTarget(i)
	original := i.command
	unregistered := 0
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 4 && args[1] == "--service-management" && args[2] == "unregister" {
			unregistered++
			for _, entry := range []struct{ directory, name string }{{i.state, "install.lock"}, {i.state, "scheduling.lock"}, {filepath.Join(i.state, "remote"), "control.lock"}} {
				busy, err := installInspectLock(entry.directory, entry.name)
				if err != nil || !busy {
					t.Fatalf("shutdown lacks %s: busy=%t %v", entry.name, busy, err)
				}
			}
			pending, err := installReadInfo(filepath.Join(i.root, "install-in-progress.json"))
			if err != nil || !pending.Equal(identity) {
				t.Fatal("shutdown lost custody", pending, err)
			}
		}
		return original(ctx, args)
	}
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	if unregistered == 0 {
		t.Fatal("removal unregistered nothing")
	}
	if !reflect.DeepEqual(originalBundle, installBundleSnapshot(t, i.app)) {
		t.Fatal("removal changed signed app bytes")
	}
	for _, status := range m.statuses {
		if status != "notRegistered" {
			t.Fatal("removal retained registration", m.statuses)
		}
	}
	if _, err := os.Lstat(filepath.Join(i.root, "install-in-progress.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removal kept its pending marker", err)
	}
}

func TestManagedRemovalPreservesDataAndVersionEvidence(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	if err := os.MkdirAll(i.state, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openQueueStore(i.state, true)
	if err != nil {
		t.Fatal(err)
	}
	store.close()
	data := map[string][]byte{
		filepath.Join(i.config, "remote.json"):             []byte(`{"enabled":true,"passwordHash":"fixture","csrfSecret":"retain"}`),
		filepath.Join(i.state, "remote/caddy/root.crt"):    []byte("fixture certificate"),
		filepath.Join(i.state, "legacy-queue-import.json"): []byte(`{"tasks":{},"groups":{}}`),
		filepath.Join(i.state, "legacy-slot.json"):         mustMetadataJSON(t, Object{"slot": filepath.Join(i.home, "fixture-execution.slot")}),
		filepath.Join(i.home, ".local/bin/other-tool"):     []byte("custom unrelated tool"),
		filepath.Join(i.root, "private-operator-note"):     []byte("retain"),
	}
	for filename, raw := range data {
		integrationTestWrite(t, filename, raw)
	}
	queueBefore := installBundleSnapshot(t, i.state)
	bundle := installBundleSnapshot(t, i.app)
	m.statuses["remote"] = "enabled"
	m.loaded["io.went.agent-remote"] = true
	managedRemovalTarget(i)
	m.calls = nil
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bundle, installBundleSnapshot(t, i.app)) {
		t.Fatal("removal modified app")
	}
	queueAfter := installBundleSnapshot(t, i.state)
	if !reflect.DeepEqual(queueBefore, queueAfter) {
		for filename, entry := range queueAfter {
			if queueBefore[filename] != entry {
				t.Errorf("removal modified queue entry %s: before=%+v after=%+v", filepath.Base(filename), queueBefore[filename], entry)
			}
		}
		t.Fatal("removal modified queue or remote state")
	}
	for filename, expected := range data {
		raw, err := os.ReadFile(filename)
		if err != nil || string(raw) != string(expected) {
			t.Fatal("removal changed retained data", filename, err)
		}
	}
	for _, filename := range []string{filepath.Join(i.home, ".local/bin/agent-heavy"), filepath.Join(i.home, ".local/bin/agent-remote"), filepath.Join(i.root, "installation.json"), filepath.Join(i.root, "install-in-progress.json")} {
		if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("removal retained generated installation file", filename, err)
		}
	}
	completed, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || completed.Build == nil || *completed.Build != 123 {
		t.Fatal("removal lost version evidence", err)
	}
	for _, status := range m.statuses {
		if status != "notRegistered" {
			t.Fatal("removal retained registration", m.statuses)
		}
	}
	if err := m.run(t); err != nil {
		t.Fatal("removal retry", err)
	}
}

func TestManagedLifecycleForeignServicesAndCustomWrappersFailBeforeChanges(t *testing.T) {
	for _, kind := range []string{"queue", "remote", "custom-wrapper", "symlink-wrapper", "lifetime-lock", "orphaned-remote"} {
		t.Run(kind, func(t *testing.T) {
			m := managedTestFixture(t)
			if err := m.run(t); err != nil {
				t.Fatal(err)
			}
			i := m.installer
			managedRemovalTarget(i)
			original := i.command
			i.command = func(ctx context.Context, args []string) ([]byte, error) {
				label := map[string]string{"queue": "io.went.agent-heavy", "remote": "io.went.agent-remote"}[kind]
				if label != "" && slices.Equal(args, []string{"launchctl", "print", i.domain() + "/" + label}) {
					return []byte("program = /foreign\narguments = {\n/foreign\n}\n"), nil
				}
				return original(ctx, args)
			}
			if kind == "custom-wrapper" {
				integrationTestWrite(t, filepath.Join(i.home, ".local/bin/agent-heavy"), []byte("#!/bin/sh\necho custom\n"))
			}
			if kind == "symlink-wrapper" {
				filename := filepath.Join(i.home, ".local/bin/agent-heavy")
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(i.app, "Contents/MacOS/agent-heavy"), filename); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "lifetime-lock" {
				m.statuses["resources"] = "notRegistered"
				delete(m.loaded, "io.went.agent-resources")
				lock, err := installLock(i.state, "resource-controller.lock")
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			if kind == "orphaned-remote" {
				integrationTestWrite(t, filepath.Join(i.state, "remote/processes.json"), []byte(`{"active":true}`))
			}
			before := installTreeWitness(t, i.home)
			m.calls = nil
			if err := m.run(t); err == nil {
				t.Fatal("unsafe lifecycle request accepted", kind)
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("unsafe lifecycle request changed home", kind)
			}
			managedRequireNoServiceMutation(t, m.calls)
		})
	}
}

func TestManagedRemovalCanUnregisterDeniedApprovalWithoutEnabling(t *testing.T) {
	m := managedOlderRegistration(t)
	m.statuses["resources"] = "requiresApproval"
	delete(m.loaded, "io.went.agent-resources")
	managedRemovalTarget(m.installer)
	m.calls = nil
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	unregisteredDenied := false
	for _, args := range m.calls {
		if len(args) == 4 && args[1] == "--service-management" {
			if args[2] == "register" {
				t.Fatal("removal tried enabling denied service", args)
			}
			unregisteredDenied = unregisteredDenied || args[2] == "unregister" && args[3] == "resources"
		}
	}
	if !unregisteredDenied {
		t.Fatal("removal retained denied registration")
	}
}

func TestManagedLifecycleIncompleteUnregistrationPreservesAppAndMetadata(t *testing.T) {
	m := managedOlderRegistration(t)
	i := m.installer
	managedRemovalTarget(i)
	bundle := installBundleSnapshot(t, i.app)
	wrappers, err := i.managedWrappers()
	if err != nil {
		t.Fatal(err)
	}
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 4 && args[1] == "--service-management" && args[2] == "unregister" && args[3] == "queue" {
			return nil, nil // Bridge returned, but registration did not change.
		}
		return original(ctx, args)
	}
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "after unregister") {
		t.Fatal("incomplete unregistration accepted", err)
	}
	if !reflect.DeepEqual(bundle, installBundleSnapshot(t, i.app)) {
		t.Fatal("incomplete unregistration changed app")
	}
	current, err := i.managedWrappers()
	if err != nil || !reflect.DeepEqual(current, wrappers) {
		t.Fatal("incomplete unregistration retired wrappers", err)
	}
	for _, filename := range []string{"installation.json", "install-in-progress.json"} {
		if _, err := os.Stat(filepath.Join(i.root, filename)); err != nil {
			t.Fatal("incomplete unregistration retired custody", filename, err)
		}
	}
}

func TestManagedRemovalPreservesPueueImportAndSlotForReinstall(t *testing.T) {
	m := managedTestFixture(t)
	m.legacy(t)
	i := m.installer
	filename := filepath.Join(i.launch, "io.went.agent-resources.plist")
	definition, err := updatePlist(filename)
	if err != nil {
		t.Fatal(err)
	}
	delete(definition, "EnvironmentVariables")
	if err := i.writePlist(filename, definition); err != nil {
		t.Fatal(err)
	}
	m.tasks["41"] = Object{"id": 41, "status": Object{"Done": Object{"result": "Success"}}}
	managedRemovalTarget(i)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{filepath.Join(i.state, "legacy-queue-import.json"), filepath.Join(i.state, "legacy-slot.json"), filepath.Join(i.root, "agent_heavy.py"), filepath.Join(i.root, ".venv/bin/python")} {
		if _, err := os.Stat(filename); err != nil {
			t.Fatal("removal lost legacy retry state", filename, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(i.home, ".local/bin/agent-heavy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removal published a wrapper", err)
	}
	i.unregisterServices = false
	if err := m.run(t); err != nil {
		t.Fatal("reinstall after Pueue removal", err)
	}
	legacy, err := ReadPrivate(filepath.Join(i.state, "legacy-queue-import.json"))
	if err != nil || obj(obj(legacy["tasks"])["41"]) == nil {
		t.Fatal("reinstall lost completed legacy history", legacy, err)
	}
}

func TestManagedPendingCandidatePinsIdentityAndRemovalRetainsOrdering(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	candidate := metadataTestInfo(t, "124")
	integrationTestWrite(t, filepath.Join(i.root, "install-in-progress.json"), mustMetadataJSON(t, candidate))
	managedRemovalTarget(i) // Installed app is still build 123, pending is 124.
	if err := m.run(t); err != nil {
		t.Fatal("explicit removal with a newer pending build", err)
	}
	retained, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || !retained.Equal(candidate) {
		t.Fatal("removal reset pending version evidence", retained, err)
	}
	i.unregisterServices = false
	before := installTreeWitness(t, i.home)
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatal("reinstallation lost anti-downgrade", err)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("downgrade refusal mutated retained data")
	}
}

func TestManagedConcurrentRemoteDisableBeforeAndDuringMigration(t *testing.T) {
	for _, phase := range []string{"before-migrate", "during-migrate"} {
		t.Run(phase, func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			if err := m.run(t); err != nil {
				t.Fatal(err)
			}
			settings := filepath.Join(i.config, "remote.json")
			integrationTestWrite(t, settings, []byte(`{"enabled":true,"passwordHash":"retain"}`))
			original := i.command
			migrated, disabled := false, false
			i.command = func(ctx context.Context, args []string) ([]byte, error) {
				// The controller serializes both migrate and explicit disable using
				// the real control.lock. A later disable must win even though the
				// install plan observed enabled=true before shutting services down.
				if slices.Equal(args, []string{filepath.Join(i.app, "Contents/MacOS/agent-remote"), "migrate"}) {
					migrated = true
					remoteState := filepath.Join(i.state, "remote")
					// A separate actor performs the explicit disable. Channels bind
					// both timings without sleeps, and every wait uses the install
					// context so a broken serialization fails within the fixture budget.
					startDisable := func(release <-chan struct{}, attempted chan<- struct{}) <-chan error {
						done := make(chan error, 1)
						go func() {
							if release != nil {
								probe, err := installLock(remoteState, "control.lock")
								if err == nil {
									probe.Close()
									close(attempted)
									done <- errors.New("disable acquired control.lock while migrate held it")
									return
								}
								close(attempted)
								select {
								case <-ctx.Done():
									done <- ctx.Err()
									return
								case <-release:
								}
							}
							lock, err := installLock(remoteState, "control.lock")
							if err != nil {
								done <- err
								return
							}
							err = installAtomic(settings, []byte(`{"enabled":false,"passwordHash":"retain"}`), 0600)
							if err == nil {
								m.statuses["remote"] = "notRegistered"
								delete(m.loaded, "io.went.agent-remote")
							}
							lock.Close()
							done <- err
						}()
						return done
					}
					awaitDisable := func(done <-chan error) error {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case err := <-done:
							disabled = err == nil
							return err
						}
					}
					if phase == "before-migrate" {
						if err := awaitDisable(startDisable(nil, nil)); err != nil {
							return nil, err
						}
					}
					lock, err := installLock(remoteState, "control.lock")
					if err != nil {
						t.Fatal("installer retained control.lock across migrate", err)
					}
					enabled, err := i.remoteEnabled()
					if err != nil {
						lock.Close()
						return nil, err
					}
					if enabled {
						m.statuses["remote"] = "enabled"
						m.loaded["io.went.agent-remote"] = true
					}
					if phase == "during-migrate" {
						release, attempted := make(chan struct{}), make(chan struct{})
						done := startDisable(release, attempted)
						select {
						case <-ctx.Done():
							lock.Close()
							close(release)
							return nil, ctx.Err()
						case <-attempted:
						}
						lock.Close()
						close(release)
						if err := awaitDisable(done); err != nil {
							return nil, err
						}
					} else {
						lock.Close()
					}
					return nil, nil
				}
				if disabled && len(args) == 4 && args[1] == "--service-management" && args[2] == "register" && args[3] == "remote" {
					t.Fatal("stale remote plan re-enabled phone access")
				}
				if migrated && len(args) == 4 && args[1] == "--service-management" && args[2] == "status" && args[3] == "remote" {
					busy, err := installInspectLock(filepath.Join(i.state, "remote"), "control.lock")
					if err != nil || !busy {
						t.Fatal("remote readback is not serialized through commit", busy, err)
					}
				}
				return original(ctx, args)
			}
			if err := m.run(t); err != nil {
				t.Fatal(err)
			}
			if !migrated || !disabled || m.statuses["remote"] != "notRegistered" || m.loaded["io.went.agent-remote"] {
				t.Fatal("remote disable was not retained", migrated, disabled, m.statuses)
			}
		})
	}
}
