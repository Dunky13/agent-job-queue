package backend

import (
	"context"
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
			if err := m.run(t); err != nil {
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
	if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(i.app, "Contents/Library/LaunchAgents/io.went.agent-heavy.plist")); err != nil {
		t.Fatal(err)
	}
	m.statuses["queue"] = "notFound"
	if err := i.managedAction(context.Background(), i.app, "register", "queue"); err == nil {
		t.Fatal("missing definition was registered")
	}
	managedRequireNoServiceMutation(t, m.calls)
}

func TestManagedNotFoundCannotPrepareReplacement(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	managedCandidate(t, m, "124")
	i.prepareReplacement = true
	m.statuses["queue"] = "notFound"
	delete(m.loaded, "io.went.agent-heavy")
	before := installTreeWitness(t, i.home)
	m.calls = nil
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "notFound") {
		t.Fatal("unresolved service lookup authorized replacement preparation", err)
	}
	managedRequireNoServiceMutation(t, m.calls)
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("unresolved service lookup changed retained installation")
	}
}

func TestManagedNotFoundRegistrationFailureIsNotSuppressed(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
		t.Fatal(err)
	}
	m.statuses["queue"] = "notFound"
	m.nativeFailure = "register queue"
	if err := i.managedAction(context.Background(), i.app, "register", "queue"); err == nil || !strings.Contains(err.Error(), "simulated native failure") {
		t.Fatal("actual registration error must propagate", err)
	}
}

func TestManagedInstallRegistersNotFoundServices(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		t.Run(map[bool]string{false: "installer", true: "copied-app"}[inPlace], func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			for service := range m.statuses {
				m.statuses[service] = "notFound"
			}
			if inPlace {
				if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
					t.Fatal(err)
				}
				i.appBundle = true
				i.payload = filepath.Join(i.app, "Contents/Resources/payload")
			}
			if err := m.run(t); err != nil {
				t.Fatal("explicit setup could not register a verified owning app", err)
			}
			for _, service := range []string{"queue", "resources", "login"} {
				if m.statuses[service] != "enabled" {
					t.Fatal("required registration was not verified", service)
				}
			}
			for _, args := range m.calls {
				if len(args) == 4 && slices.Equal(args[1:], []string{"--service-management", "register", "remote"}) {
					t.Fatal("disabled phone access was registered")
				}
			}
		})
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
				if len(args) == 4 && slices.Equal(args[1:], []string{"--service-management", "status", "remote"}) {
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
