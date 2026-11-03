package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestManagedRegisterVerifiedServiceAfterNotFound(t *testing.T) {
	for _, service := range []string{"queue", "resources", "login"} {
		t.Run(service, func(t *testing.T) {
			m := managedTestFixture(t)
			m.statuses[service] = "notFound"
			integrationTestWrite(t, filepath.Join(m.installer.root, "install-in-progress.json"), integrationTestRead(t, filepath.Join(m.installer.app, "Contents/Resources/build-info.json")))
			if err := m.installer.managedAction(context.Background(), m.installer.app, "register", service); err != nil {
				t.Fatal("verified service was refused before registration", err)
			}
			calls := 0
			for _, args := range m.calls {
				if len(args) == 4 && slices.Equal(args[1:], []string{"--service-management", "register", service}) {
					calls++
				}
			}
			if calls != 1 || m.statuses[service] != "enabled" {
				t.Fatal("registration must be attempted once and verified", calls, m.statuses)
			}
		})
	}
}

func TestManagedNotFoundCannotRegisterMissingDefinition(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := os.Remove(filepath.Join(i.app, "Contents/Library/LaunchAgents/io.went.agent-heavy.plist")); err != nil {
		t.Fatal(err)
	}
	m.statuses["queue"] = "notFound"
	if err := i.managedAction(context.Background(), i.app, "register", "queue"); err == nil {
		t.Fatal("missing definition was registered")
	}
	managedRequireNoServiceMutation(t, m.calls)
}

func TestManagedNotFoundStillLoadedCannotBeRemoved(t *testing.T) {
	for _, service := range []string{"queue", "resources", "remote"} {
		t.Run(service, func(t *testing.T) {
			m := managedTestFixture(t)
			if err := m.run(t); err != nil {
				t.Fatal(err)
			}
			i := m.installer
			managedRemovalTarget(i)
			m.statuses[service] = "notFound"
			m.loaded[map[string]string{"queue": "io.went.agent-heavy", "resources": "io.went.agent-resources", "remote": "io.went.agent-remote"}[service]] = true
			before := installTreeWitness(t, i.home)
			m.calls = nil
			if err := m.run(t); err == nil {
				t.Fatal("a loaded service was treated as never registered")
			}
			managedRequireNoServiceMutation(t, m.calls)
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("refusal changed the retained installation")
			}
		})
	}
}

func TestManagedRemovalAfterUnregisterAcceptsNotFoundOnlyWhenUnloaded(t *testing.T) {
	for _, loaded := range []bool{false, true} {
		t.Run(fmt.Sprintf("loaded=%v", loaded), func(t *testing.T) {
			m := managedOlderRegistration(t)
			i := m.installer
			managedRemovalTarget(i)
			original := i.command
			i.command = func(ctx context.Context, args []string) ([]byte, error) {
				// A later lookup can miss a service that unregistered successfully.
				if len(args) == 3 && args[1] == "--prepare-app-replacement" {
					m.statuses["queue"] = "notFound"
					if loaded {
						m.loaded["io.went.agent-heavy"] = true
					}
				}
				return original(ctx, args)
			}
			err := m.run(t)
			if loaded {
				if err == nil || !strings.Contains(err.Error(), "still loaded") {
					t.Fatal("a loaded service passed the final check", err)
				}
				if _, statErr := os.Stat(filepath.Join(i.root, "installation.json")); statErr != nil {
					t.Fatal("refusal retired installation custody", statErr)
				}
			} else if err != nil {
				t.Fatal("a lookup miss after unregistering blocked removal", err)
			}
		})
	}
}

// What a real Mac reports for an app that supervises its own helpers: launchd
// holds only the login item, and the other services have never been registered.
func TestManagedRemovalOfSupervisedApp(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	for _, service := range []string{"queue", "resources", "remote"} {
		m.statuses[service] = "notFound"
	}
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	store, err := openQueueStore(i.state, true)
	if err != nil {
		t.Fatal(err)
	}
	store.close()
	bundle := installBundleSnapshot(t, i.app)
	private := integrationTestRead(t, QueueDatabasePath(i.state))
	managedRemovalTarget(i)

	// A running app's daemon holds its lifetime lock and launchd knows nothing of
	// it, so removal refuses before it reaches its quit step.
	held, err := installLock(i.state, "queue-daemon.lock")
	if err != nil {
		t.Fatal(err)
	}
	before := installTreeWitness(t, i.home)
	m.calls = nil
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "lifetime lock") {
		t.Fatal("removal ran beside a running daemon", err)
	}
	managedRequireNoServiceMutation(t, m.calls)
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("refusal changed the installation")
	}
	held.Close()

	// Removal also refuses a queue that still owes work after the app quit.
	store, err = openQueueStore(i.state, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(41,'fixture','{}','running')"); err != nil {
		t.Fatal(err)
	}
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "drained") {
		t.Fatal("removal skipped the drain gate", err)
	}
	if _, err := store.db.Exec("DELETE FROM queue_jobs WHERE id = 41"); err != nil {
		t.Fatal(err)
	}
	store.close()

	m.calls = nil
	if err := m.run(t); err != nil {
		t.Fatal("documented removal failed", err)
	}
	if got := managedServiceCalls(m, "unregister"); !slices.Equal(got, []string{"login"}) {
		t.Fatalf("unregistered %v, want only the login item", got)
	}
	if !slices.ContainsFunc(m.calls, func(args []string) bool { return len(args) == 3 && args[1] == "--prepare-app-replacement" }) {
		t.Fatal("removal did not ask a running app to quit")
	}
	if m.statuses["login"] != "notRegistered" {
		t.Fatal("removal retained the login item", m.statuses)
	}
	for _, filename := range []string{filepath.Join(i.home, ".local/bin/agent-heavy"), filepath.Join(i.home, ".local/bin/agent-remote"), filepath.Join(i.root, "installation.json"), filepath.Join(i.root, "install-in-progress.json")} {
		if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("removal retained", filename, err)
		}
	}
	if !reflect.DeepEqual(bundle, installBundleSnapshot(t, i.app)) {
		t.Fatal("removal changed the app")
	}
	if len(integrationTestRead(t, QueueDatabasePath(i.state))) != len(private) {
		t.Fatal("removal changed the queue store")
	}

	// Removal keeps the build number, so an untracked build still cannot go over it.
	i.unregisterServices = false
	untracked := metadataTestInfo(t, "123")
	untracked.Build, untracked.Commit = nil, nil
	for _, filename := range []string{"Contents/Resources/build-info.json", "Contents/Resources/payload/source/build-info.json"} {
		integrationTestWrite(t, filepath.Join(i.app, filename), mustMetadataJSON(t, untracked))
	}
	info, err := updatePlist(filepath.Join(i.app, "Contents/Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	info["CFBundleVersion"] = "0"
	if err := i.writePlist(filepath.Join(i.app, "Contents/Info.plist"), info); err != nil {
		t.Fatal(err)
	}
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "untracked") {
		t.Fatal("an untracked build replaced the retained build number", err)
	}
	// The documented route for a local build: a tracked number above the retained one.
	managedCandidate(t, m, "124")
	if err := m.run(t); err != nil {
		t.Fatal("a newer tracked build could not reinstall after removal", err)
	}
}

func TestManagedNotFoundRegistrationFailureIsNotSuppressed(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	m.statuses["queue"] = "notFound"
	m.nativeFailure = "register queue"
	if err := i.managedAction(context.Background(), i.app, "register", "queue"); err == nil || !strings.Contains(err.Error(), "simulated native failure") {
		t.Fatal("actual registration error must propagate", err)
	}
}

func TestManagedInstallRegistersNotFoundServices(t *testing.T) {
	m := managedTestFixture(t)
	for service := range m.statuses {
		m.statuses[service] = "notFound"
	}
	if err := m.run(t); err != nil {
		t.Fatal("explicit setup could not register a verified owning app", err)
	}
	// The menu bar app supervises the helpers; only its login item registers.
	if m.statuses["login"] != "enabled" {
		t.Fatal("required registration was not verified")
	}
	for _, args := range m.calls {
		if len(args) == 4 && args[1] == "--service-management" && args[2] == "register" && args[3] != "login" {
			t.Fatal("setup registered a service it must not", args)
		}
	}
}

func TestManagedNotFoundRemoteCannotHideOwnedWork(t *testing.T) {
	for _, kind := range []string{"loaded-job", "generation"} {
		t.Run(kind, func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			m.statuses["remote"] = "notFound"
			original := i.command
			i.command = func(ctx context.Context, args []string) ([]byte, error) {
				_, pending := os.Stat(filepath.Join(i.root, "install-in-progress.json"))
				if pending == nil && len(args) == 4 && slices.Equal(args[1:], []string{"--service-management", "status", "remote"}) {
					if kind == "loaded-job" {
						m.loaded["io.went.agent-remote"] = true
					} else {
						integrationTestWrite(t, filepath.Join(i.state, "remote/processes.json"), []byte(`{"active":true}`))
					}
				}
				return original(ctx, args)
			}
			if err := m.run(t); err == nil || !strings.Contains(err.Error(), "owned work remains") {
				t.Fatal("unresolved remote lookup hid existing work", err)
			}
			if _, err := os.Stat(filepath.Join(i.root, "install-in-progress.json")); err != nil {
				t.Fatal("incomplete remote shutdown lost pending identity", err)
			}
		})
	}
}
