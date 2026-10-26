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
	"time"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
)

func TestManagedInPlaceRetryRetiresPublicationCustody(t *testing.T) {
	m := managedRetainedBackupFixture(t)
	i := m.installer
	payload := i.payload
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	if err := m.run(t); err != nil {
		t.Fatal("in-place completion failed", err)
	}
	for _, path := range []string{i.managedReceiptPath(), i.managedPreviousPath(), filepath.Join(i.root, "install-in-progress.json")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("successful in-place completion retained publication custody", path, err)
		}
	}
	i.appBundle, i.payload = false, payload
	managedCandidate(t, m, "125")
	if err := m.run(t); err != nil {
		t.Fatal("in-place completion blocked subsequent payload upgrade", err)
	}
}

func TestManagedInPlaceRetryRefusesSubstitutedBackupBeforeMutation(t *testing.T) {
	m := managedRetainedBackupFixture(t)
	i := m.installer
	previous := i.managedPreviousPath()
	if err := os.Rename(previous, previous+".original"); err != nil {
		t.Fatal(err)
	}
	if err := installCopyTree(previous+".original", previous); err != nil {
		t.Fatal(err)
	}
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	before := installTreeWitness(t, i.home)
	m.calls = nil
	if err := m.run(t); err == nil {
		t.Fatal("in-place completion accepted a same-content replacement backup")
	}
	managedRequireNoServiceMutation(t, m.calls)
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("refusal changed publication evidence")
	}
}

func TestManagedInPlaceRetryRefusesUnwritableBackupParentBeforeMutation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write owner-read-only directories")
	}
	m := managedRetainedBackupFixture(t)
	i := m.installer
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	parent := filepath.Dir(i.app)
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
	before := installTreeWitness(t, i.home)
	m.calls = nil
	if err := m.run(t); err == nil {
		t.Fatal("in-place retry accepted backup cleanup it could not finish")
	}
	managedRequireNoServiceMutation(t, m.calls)
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("unwritable backup parent refusal changed installation")
	}
}

func TestManagedLifecycleDoesNotStrandPublicationCustody(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		for _, removal := range []bool{false, true} {
			t.Run(fmt.Sprintf("upgrade=%t/removal=%t", upgrade, removal), func(t *testing.T) {
				m := managedTestFixture(t)
				i := m.installer
				payload := i.payload
				if upgrade {
					if err := m.run(t); err != nil {
						t.Fatal(err)
					}
					managedCandidate(t, m, "124")
				}
				m.nativeFailure = "register resources"
				if err := m.run(t); err == nil {
					t.Fatal("expected partial publication")
				}
				m.nativeFailure = ""
				i.prepareReplacement = true
				if removal {
					managedRemovalTarget(i)
				}
				before := installTreeWitness(t, i.home)
				m.calls = nil
				err := m.run(t)
				if removal && !upgrade {
					if err != nil {
						t.Fatal("candidate-only removal failed", err)
					}
					for _, path := range []string{i.managedReceiptPath(), filepath.Join(i.root, "install-in-progress.json")} {
						if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
							t.Fatal("removal stranded candidate-only publication custody", path, err)
						}
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), "pending publication") {
						t.Fatal("lifecycle operation authorized unresolved publication", err)
					}
					managedRequireNoServiceMutation(t, m.calls)
					if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
						t.Fatal("refusal changed retained publication evidence")
					}
					// Complete the exact pending candidate through its supported retry.
					i.prepareReplacement, i.unregisterServices, i.appBundle = false, false, true
					i.payload = filepath.Join(i.app, "Contents/Resources/payload")
					if err := m.run(t); err != nil {
						t.Fatal("exact pending completion failed", err)
					}
				}
				i.prepareReplacement, i.unregisterServices, i.appBundle = false, false, false
				i.payload = payload
				if upgrade {
					managedCandidate(t, m, "125")
				}
				if err := m.run(t); err != nil {
					t.Fatal("resolved lifecycle custody blocked subsequent installation", err)
				}
			})
		}
	}
}

func TestManagedInstallerComposesRemoteAndNativeDeadlines(t *testing.T) {
	outer, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, action := range []string{"disable", "migrate"} {
		remote, stop := installCommandContext(outer, []string{"/Applications/Agent Queue.app/Contents/MacOS/agent-remote", action})
		defer stop()
		deadline, ok := remote.Deadline()
		if !ok || time.Until(deadline) < appservices.ControllerLifecycleTimeout {
			t.Fatal("installer truncates remote control lifecycle", action)
		}
		controller, finish := context.WithTimeout(remote, appservices.ControllerLifecycleTimeout)
		defer finish()
		native, release := installCommandContext(controller, []string{"/Applications/Agent Queue.app/Contents/MacOS/AgentQueue", "--service-management", "unregister", "remote"})
		defer release()
		nativeDeadline, ok := native.Deadline()
		if !ok || time.Until(nativeDeadline) < appservices.NativeCommandTimeout-time.Second {
			t.Fatal("composed lifecycle truncates native completion")
		}
	}
}

func TestManagedPayloadRefusesUnwritableAppBeforeStoppingServices(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write owner-read-only directories")
	}
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	directory := filepath.Join(i.app, "Contents")
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, info.Mode().Perm()) })
	before := installTreeWitness(t, i.home)
	m.calls = nil
	if err := m.run(t); err == nil {
		t.Fatal("unwritable application was replaced")
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("unwritable app refusal changed installation")
	}
	managedRequireNoServiceMutation(t, m.calls)
}

func TestManagedPayloadResolvesBothRecordedApplicationsLocations(t *testing.T) {
	for _, system := range []bool{false, true} {
		t.Run(fmt.Sprint(system), func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			app := filepath.Join(i.home, "Applications/Finder Copied Queue.app")
			if system {
				app = "/Applications/Finder Copied Queue.app"
			}
			integrationTestWrite(t, filepath.Join(i.root, "installation.json"), mustMetadataJSON(t, Object{"schemaVersion": 1, "serviceManagement": true, "app": app}))
			before := installTreeWitness(t, i.home)
			if err := i.resolvePayloadApp(); err != nil || i.app != app {
				t.Fatal("recorded Applications target was not selected", i.app, err)
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) || len(m.calls) != 0 {
				t.Fatal("resolving a recorded target changed state or invoked lifecycle")
			}
			i.appBundle, i.app = true, filepath.Join(i.home, "Applications/Explicit Queue.app")
			if err := i.resolvePayloadApp(); err != nil || i.app == app {
				t.Fatal("recorded metadata redirected an explicit app-bundle target", err)
			}
		})
	}
}

func TestManagedPayloadUpgradeAndPreflightUseRecordedTarget(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	payload, defaultApp := i.payload, i.app
	i.app = filepath.Join(i.home, "Applications/Finder Copied Queue.app")
	if err := installCopyTree(filepath.Join(payload, "Agent Queue.app"), i.app); err != nil {
		t.Fatal(err)
	}
	recorded := i.app
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i.appBundle, i.payload, i.app = false, payload, defaultApp
	before := installTreeWitness(t, i.home)
	report := i.preflight(context.Background())
	if len(report.Issues) != 0 || i.app != recorded || !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("payload preflight did not read the recorded target without mutation", report, i.app)
	}
	i.app = defaultApp
	if err := m.run(t); err != nil || i.app != recorded {
		t.Fatal("ordinary payload upgrade lost recorded target", i.app, err)
	}
	if _, err := os.Stat(defaultApp); !os.IsNotExist(err) {
		t.Fatal("payload upgrade created a second app at its default location", err)
	}
}

func TestManagedReadinessUsesPublicStatusSchema(t *testing.T) {
	value := Object{"updatedAt": time.Now().UTC().Format(time.RFC3339Nano), "queue": Object{"status": "running", "limit": 1, "running": 0, "waiting": 0}, "jobs": []any{}, "scheduler": Object{}}
	if err := managedReadiness(mustMetadataJSON(t, value)); err != nil {
		t.Fatal("public CLI status was refused", err)
	}
	for _, invalid := range []Object{
		{"tasks": Object{}, "groups": Object{}},
		{"updatedAt": value["updatedAt"], "queue": value["queue"], "jobs": nil, "scheduler": Object{}},
		{"updatedAt": "invalid", "queue": value["queue"], "jobs": []any{}, "scheduler": Object{}},
		{"updatedAt": value["updatedAt"], "queue": Object{"status": "running", "limit": 0, "running": 0, "waiting": 0}, "jobs": []any{}, "scheduler": Object{}},
		{"updatedAt": value["updatedAt"], "queue": value["queue"], "jobs": []any{}},
	} {
		if err := managedReadiness(mustMetadataJSON(t, invalid)); err == nil {
			t.Fatal("invalid/private status certified readiness", invalid)
		}
	}
}

func TestManagedPublicationKeepsPendingIdentityAuthoritative(t *testing.T) {
	for _, changed := range []string{"candidate", "installed"} {
		t.Run(changed, func(t *testing.T) {
			m := managedTestFixture(t)
			if err := m.run(t); err != nil {
				t.Fatal(err)
			}
			i := m.installer
			managedCandidate(t, m, "124")
			mutated := false
			original := i.command
			i.command = func(ctx context.Context, args []string) ([]byte, error) {
				last := args[len(args)-1]
				if !mutated && args[0] == "/usr/bin/codesign" && strings.HasSuffix(last, ".app") && strings.Contains(last, ".Agent Queue.installing-") {
					mutated = true
					target := last
					if changed == "installed" {
						target = i.app
					}
					identity := metadataTestInfo(t, "125")
					for _, file := range []string{"Contents/Resources/build-info.json", "Contents/Resources/payload/source/build-info.json"} {
						integrationTestWrite(t, filepath.Join(target, file), mustMetadataJSON(t, identity))
					}
					info, err := updatePlist(filepath.Join(target, "Contents/Info.plist"))
					if err != nil {
						t.Fatal(err)
					}
					info["CFBundleVersion"] = "125"
					if err := i.writePlist(filepath.Join(target, "Contents/Info.plist"), info); err != nil {
						t.Fatal(err)
					}
				}
				return original(ctx, args)
			}
			if err := m.run(t); err == nil || !mutated {
				t.Fatal("publication accepted identity changed during staging", changed, err)
			}
			data, err := installReadPayload(i.app, filepath.Join(i.app, "Contents/Resources/build-info.json"), 16384, true)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := ValidateBuildInfo(data)
			want := 123
			if changed == "installed" {
				want = 125
			}
			if err != nil || actual.Build == nil || *actual.Build != want {
				t.Fatal("conflicting staging replaced the existing app", actual, err)
			}
			for _, service := range []string{"queue", "resources", "login"} {
				if m.statuses[service] != "notRegistered" {
					t.Fatal("conflicting staging activated a service", service, m.statuses[service])
				}
			}
		})
	}
}

func TestManagedRefusedMenuShutdownPreservesPreviousBundle(t *testing.T) {
	for _, response := range []string{"refused", "invalid-json", "wrong-app", "wrong-schema"} {
		for _, operation := range []string{"install", "prepare", "remove"} {
			t.Run(response+"/"+operation, func(t *testing.T) {
				m := managedTestFixture(t)
				if err := m.run(t); err != nil {
					t.Fatal(err)
				}
				i := m.installer
				if operation == "prepare" {
					i.prepareReplacement = true
				} else if operation == "remove" {
					managedRemovalTarget(i)
				}
				before := installBundleSnapshot(t, i.app)
				original := i.command
				i.command = func(ctx context.Context, args []string) ([]byte, error) {
					if len(args) == 3 && args[1] == "--prepare-app-replacement" {
						switch response {
						case "refused":
							return nil, fmt.Errorf("menu refused termination")
						case "invalid-json":
							return []byte("invalid"), nil
						case "wrong-app":
							return mustMetadataJSON(t, Object{"schemaVersion": 1, "app": "/Applications/Other.app", "status": "stopped"}), nil
						case "wrong-schema":
							return mustMetadataJSON(t, Object{"schemaVersion": 2, "app": i.app, "status": "stopped"}), nil
						}
					}
					return original(ctx, args)
				}
				if err := m.run(t); err == nil {
					t.Fatal("unconfirmed menu shutdown allowed replacement")
				}
				if !reflect.DeepEqual(before, installBundleSnapshot(t, i.app)) {
					t.Fatal("bundle changed after unconfirmed UI shutdown")
				}
				if _, err := os.Stat(filepath.Join(i.root, "install-in-progress.json")); err != nil {
					t.Fatal("refused shutdown lost pending identity", err)
				}
			})
		}
	}
}

func TestManagedDirectInstallRefusesForeignWrappersBeforeMutation(t *testing.T) {
	for _, appBundle := range []bool{false, true} {
		for _, name := range []string{"agent-heavy", "agent-remote"} {
			for _, symlink := range []bool{false, true} {
				t.Run(fmt.Sprintf("bundle=%v/%s/symlink=%v", appBundle, name, symlink), func(t *testing.T) {
					m := managedTestFixture(t)
					i := m.installer
					if appBundle {
						if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
							t.Fatal(err)
						}
						i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
					}
					wrapper := filepath.Join(i.home, ".local/bin", name)
					integrationTestWrite(t, wrapper, []byte("#!/bin/sh\necho custom\n"))
					if symlink {
						if err := os.Rename(wrapper, wrapper+".custom"); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(wrapper+".custom", wrapper); err != nil {
							t.Fatal(err)
						}
					}
					before := installTreeWitness(t, i.home)
					if err := m.run(t); err == nil {
						t.Fatal("foreign wrapper accepted")
					}
					if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
						t.Fatal("wrapper refusal changed installation")
					}
					managedRequireNoServiceMutation(t, m.calls)
				})
			}
		}
	}
}

func TestManagedDirectInstallRechecksWrappersUnderLocks(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	calls := 0
	i.queueSnapshot = func(context.Context) (Object, error) {
		calls++
		if calls == 1 {
			integrationTestWrite(t, filepath.Join(i.home, ".local/bin/agent-heavy"), []byte("custom during planning"))
		}
		return m.snapshot(), nil
	}
	// Force the drained legacy snapshot so the concurrent fixture edit occurs
	// after the first wrapper inspection and before the locked plan.
	for _, directory := range []string{i.launch, i.state} {
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
	m.loaded["io.went.agent-heavy"] = true
	if err := m.run(t); err == nil || calls == 0 {
		t.Fatal("locked planning accepted a changed wrapper", calls, err)
	}
	managedRequireNoServiceMutation(t, m.calls)
	if _, err := os.Stat(filepath.Join(i.root, "install-in-progress.json")); !os.IsNotExist(err) {
		t.Fatal("wrapper race published a pending identity", err)
	}
}

func TestManagedHistoricalCaskRemovedMenuDefinition(t *testing.T) {
	for _, kind := range []string{"canonical", "foreign-program", "foreign-app", "extra-argument"} {
		t.Run(kind, func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			for _, directory := range []string{i.launch, i.state} {
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
			// This is the predecessor cask's actual deletion before successor
			// preflight. Its loaded command remains in the user's launchd domain.
			if err := os.Remove(filepath.Join(i.launch, "io.went.agent-queue.plist")); err != nil {
				t.Fatal(err)
			}
			m.loaded["io.went.agent-queue"] = true
			program, app, extra := "/usr/bin/open", i.app, ""
			switch kind {
			case "foreign-program":
				program = "/foreign/open"
			case "foreign-app":
				app = filepath.Join(i.home, "Other.app")
			case "extra-argument":
				extra = "--args\n"
			}
			original := i.command
			i.command = func(ctx context.Context, args []string) ([]byte, error) {
				if m.loaded["io.went.agent-queue"] && slices.Equal(args, []string{"launchctl", "print", i.domain() + "/io.went.agent-queue"}) {
					return []byte("program = " + program + "\narguments = {\n/usr/bin/open\n-a\n" + app + "\n" + extra + "}\n"), nil
				}
				return original(ctx, args)
			}
			before := installTreeWitness(t, i.home)
			err := m.run(t)
			if kind == "canonical" {
				if err != nil || m.loaded["io.went.agent-queue"] {
					t.Fatal("historical loaded menu was not safely retired", err)
				}
			} else {
				if err == nil || !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
					t.Fatal("foreign orphaned menu was changed", err)
				}
				managedRequireNoServiceMutation(t, m.calls)
			}
		})
	}
}
