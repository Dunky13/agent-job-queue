package backend

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"howett.net/plist"
)

func TestInstallerAppBundleCLIValidatesBundleBeforeSetup(t *testing.T) {
	f := installTestFixture(t)
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_MODE", "1")
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_TOOLS", f.installer.path)
	// A recognized mode with a missing bundle is an installation error, not usage.
	if code := InstallMain([]string{"--app-bundle", f.installer.app}); code != 1 {
		t.Fatalf("app-bundle mode was not recognized: exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(f.installer.root, "install-in-progress.json")); !os.IsNotExist(err) {
		t.Fatalf("invalid bundle started setup: %v", err)
	}
}

func (f *installFixture) appBundle(t *testing.T) {
	t.Helper()
	install := f.installer
	if err := installCopyTree(filepath.Join(install.payload, "Agent Queue.app"), install.app); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(install.app, "Contents/Resources/payload")
	if err := installCopyTree(filepath.Join(install.payload, "source"), filepath.Join(payload, "source")); err != nil {
		t.Fatal(err)
	}
	install.payload = payload
	install.appBundle = true
}

type installBundleEntry struct {
	Identity integrationIdentity
	Digest   [sha256.Size]byte
}

func installBundleSnapshot(t *testing.T, root string) map[string]installBundleEntry {
	t.Helper()
	result := map[string]installBundleEntry{}
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Lstat(filename, &stat); err != nil {
			return err
		}
		item := installBundleEntry{Identity: integrationStatIdentity(&stat)}
		if entry.Type().IsRegular() {
			file, err := os.Open(filename)
			if err != nil {
				return err
			}
			hash := sha256.New()
			_, err = io.Copy(hash, file)
			file.Close()
			if err != nil {
				return err
			}
			copy(item.Digest[:], hash.Sum(nil))
		}
		result[filename] = item
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertInstallerNoAppCommands(t *testing.T, f *installFixture) {
	t.Helper()
	for _, args := range f.calls {
		if slices.Contains([]string{"osascript", "open", "ditto"}, filepath.Base(args[0])) {
			t.Fatalf("self-install touched the running application: %v", args)
		}
		if strings.Contains(strings.Join(args, " "), "python") {
			t.Fatalf("self-install executed legacy Python: %v", args)
		}
	}
}

func TestInstallerAppBundlePreservesApplicationAndLaunchesSuppliedPath(t *testing.T) {
	f := installTestFixture(t)
	f.appBundle(t)
	install := f.installer
	// The app's basename may be changed by Finder. Its installed path is authoritative.
	app := filepath.Join(install.home, "Applications/Agent Queue Renamed.app")
	if err := os.Rename(install.app, app); err != nil {
		t.Fatal(err)
	}
	install.app, install.payload = app, filepath.Join(app, "Contents/Resources/payload")
	if runtime.GOOS == "darwin" {
		// Exercise production branches too; all launchctl calls remain simulated.
		install.testing = false
	}
	for _, name := range []string{".Agent Queue.installing.app", ".Agent Queue.previous.app"} {
		integrationTestWrite(t, filepath.Join(install.home, "Applications", name, "keep"), []byte("unrelated recovery artifact"))
	}
	before := installBundleSnapshot(t, filepath.Join(install.home, "Applications"))
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if after := installBundleSnapshot(t, filepath.Join(install.home, "Applications")); !reflect.DeepEqual(before, after) {
		t.Fatal("self-install changed application bytes, ownership, mode, inode, timestamps, or sibling recovery artifacts")
	}
	assertInstallerNoAppCommands(t, f)
	launch, err := updatePlist(filepath.Join(install.launch, "io.went.agent-queue.plist"))
	if err != nil {
		t.Fatal(err)
	}
	args, err := stringsFrom(launch["ProgramArguments"])
	if err != nil || !slices.Equal(args, []string{"/usr/bin/open", "-a", app}) {
		t.Fatalf("launch agent targets another application: %v %v", args, err)
	}
	if _, err := updateInstalled(install.root, app); err != nil {
		t.Fatalf("setup did not complete: %v", err)
	}
}

func TestInstallerAppBundleLegacyCallbackMigrationAndFailureRetry(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	f.appBundle(t)
	install := f.installer
	configuration := install.freshConfig()
	configuration["daemon"].(Object)["callback"] = "/opt/homebrew/bin/python3 " + installQuote(filepath.Join(install.root, "agent_heavy.py")) + " cleanup {{ id }}"
	integrationTestWrite(t, filepath.Join(install.config, "pueue.yml"), mustMetadataJSON(t, configuration))
	f.tasks = Object{"17": Object{"status": Object{"Done": Object{"result": "Success"}}, "command": "completed fixture"}}
	before := installBundleSnapshot(t, install.app)
	f.fail = "bootstrap "
	if err := f.run(t); err == nil {
		t.Fatal("failed daemon bootstrap reported completion")
	}
	if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); err != nil {
		t.Fatalf("failed setup did not retain retry marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(install.root, "build-info.json")); !os.IsNotExist(err) {
		t.Fatal("failed setup advanced installed identity")
	}
	f.fail = ""
	if err := f.run(t); err != nil {
		t.Fatalf("setup could not retry after backend publication: %v", err)
	}
	config, err := installConfig(filepath.Join(install.config, "pueue.yml"))
	if err != nil {
		t.Fatal(err)
	}
	callback, err := installConfigCallback(config)
	if err != nil || callback != installQuote(filepath.Join(install.root, "bin/agent-heavy"))+" cleanup {{ id }}" {
		t.Fatalf("legacy callback not migrated: %q %v", callback, err)
	}
	if _, err := os.Stat(filepath.Join(install.root, "agent_heavy.py")); !os.IsNotExist(err) {
		t.Fatal("legacy Python runtime not retired")
	}
	snapshot, err := ReadPrivate(filepath.Join(install.state, "legacy-queue-import.json"))
	if err != nil || text(obj(obj(snapshot["tasks"])["17"])["command"]) != "completed fixture" {
		t.Fatalf("queue history was not preserved: %v %v", snapshot, err)
	}
	if after := installBundleSnapshot(t, install.app); !reflect.DeepEqual(before, after) {
		t.Fatal("migration or retry changed the application")
	}
	assertInstallerNoAppCommands(t, f)
}

func TestInstallerAppBundleUpgradePreservesActiveGoJobs(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.installer.app); err != nil {
		t.Fatal(err)
	}
	f.appBundle(t)
	private := filepath.Join(f.installer.state, "jobs/active-job.json")
	integrationTestWrite(t, private, []byte("active Go job"))
	f.tasks = Object{"8": Object{"status": Object{"Running": Object{}}, "command": "active Go job"}}
	f.calls = nil
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	for _, args := range f.calls {
		if args[0] == "launchctl" && (args[1] == "bootout" || (args[1] == "kickstart" && strings.HasSuffix(args[len(args)-1], "/io.went.agent-heavy"))) {
			t.Fatalf("self-install interrupted the Go daemon: %v", args)
		}
	}
	if !bytes.Equal(integrationTestRead(t, private), []byte("active Go job")) {
		t.Fatal("active job state changed")
	}
	assertInstallerNoAppCommands(t, f)
}

func TestInstallerAppBundleCLISyntax(t *testing.T) {
	for _, args := range [][]string{nil, {"--app-bundle"}, {"--app-bundle", "relative.app"}, {"--payload", "relative"}, {"--app-bundle", "/Applications/Agent Queue.app", "--payload", "/payload"}} {
		if code := InstallMain(args); code != 2 {
			t.Fatalf("invalid invocation %v returned %d", args, code)
		}
	}
}

func TestInstallerAppBundleRejectsUnsafePathsBeforeSetup(t *testing.T) {
	for _, kind := range []string{"downloads", "disk-image", "arbitrary", "nested", "traversal", "dot", "relative", "symlink-app", "symlink-parent", "symlink-payload", "symlink-source", "symlink-binary", "writable-binary", "hardlink", "optional-symlink", "writable-directory", "recursive-app"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.appBundle(t)
			install := f.installer
			filename := filepath.Join(install.payload, "source/bin/agent-heavy")
			switch kind {
			case "downloads", "disk-image", "arbitrary", "nested", "traversal", "dot", "relative":
				paths := map[string]string{
					"downloads":  filepath.Join(install.home, "Downloads/Agent Queue.app"),
					"disk-image": "/Volumes/Agent Queue/Agent Queue.app",
					"arbitrary":  filepath.Join(install.home, "Desktop/Agent Queue.app"),
					"nested":     filepath.Join(install.home, "Applications/nested/Agent Queue.app"),
					"traversal":  install.home + "/Downloads/../Applications/Agent Queue.app",
					"dot":        install.home + "/Applications/./Agent Queue.app",
					"relative":   "Applications/Agent Queue.app",
				}
				install.app = paths[kind]
				install.payload = filepath.Join(install.app, "Contents/Resources/payload")
			case "symlink-app", "symlink-parent", "symlink-payload", "symlink-source", "symlink-binary":
				switch kind {
				case "symlink-app":
					filename = install.app
				case "symlink-parent":
					filename = filepath.Dir(install.app)
				case "symlink-payload":
					filename = install.payload
				case "symlink-source":
					filename = filepath.Join(install.payload, "source")
				}
				outside := filepath.Join(install.home, "outside")
				if err := os.Rename(filename, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filename); err != nil {
					t.Fatal(err)
				}
			case "writable-binary":
				if err := os.Chmod(filename, 0775); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filename, filepath.Join(install.home, "binary-link")); err != nil {
					t.Fatal(err)
				}
			case "optional-symlink":
				// Even unconsumed payload entries must not cross the bundle boundary.
				outside := filepath.Join(install.home, "outside")
				integrationTestWrite(t, outside, []byte("outside payload"))
				if err := os.Symlink(outside, filepath.Join(install.payload, "source/unused")); err != nil {
					t.Fatal(err)
				}
			case "writable-directory":
				if err := os.Mkdir(filepath.Join(install.payload, "source/unused"), 0777); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(install.payload, "source/unused"), 0777); err != nil {
					t.Fatal(err)
				}
			case "recursive-app":
				integrationTestWrite(t, filepath.Join(install.payload, "Agent Queue.app/unexpected"), nil)
			}
			if err := f.run(t); err == nil {
				t.Fatal("unsafe app or payload accepted")
			}
			if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); !os.IsNotExist(err) {
				t.Fatalf("unsafe payload started a transaction: %v", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("unsafe payload invoked service commands: %v", f.calls)
			}
		})
	}
}

func TestInstallerAppBundleIdentityFences(t *testing.T) {
	for _, kind := range []string{"support-mismatch", "bundle-version", "newer-installed", "newer-pending", "same-build-conflict", "private-marker-symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.appBundle(t)
			install := f.installer
			newer := metadataTestInfo(t, "124")
			switch kind {
			case "support-mismatch":
				integrationTestWrite(t, filepath.Join(install.payload, "source/build-info.json"), mustMetadataJSON(t, newer))
			case "bundle-version":
				updateTestBundle(t, install.app, "io.went.agent-queue", newer, plist.XMLFormat)
			case "newer-installed", "newer-pending", "same-build-conflict", "private-marker-symlink":
				marker := filepath.Join(install.root, "build-info.json")
				if kind == "newer-pending" {
					marker = filepath.Join(install.root, "install-in-progress.json")
				}
				if kind == "same-build-conflict" {
					newer = metadataTestInfo(t, "123")
					commit := strings.Repeat("c", 40)
					newer.Commit = &commit
				}
				integrationTestWrite(t, marker, mustMetadataJSON(t, newer))
				if kind == "private-marker-symlink" {
					if err := os.Rename(marker, marker+".target"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(marker+".target", marker); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := installBundleSnapshot(t, install.app)
			if err := f.run(t); err == nil {
				t.Fatal("invalid or older bundle accepted")
			}
			if _, err := os.Stat(filepath.Join(install.root, "bin/agent-heavy")); !os.IsNotExist(err) {
				t.Fatal("identity failure published backend")
			}
			if after := installBundleSnapshot(t, install.app); !reflect.DeepEqual(before, after) {
				t.Fatal("identity failure changed the app")
			}
		})
	}
}

func TestInstallerAppBundlePrivatePathsRemainStrict(t *testing.T) {
	for _, kind := range []string{"state-symlink", "lock-symlink", "config-symlink", "config-writable", "marker-writable"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.appBundle(t)
			install := f.installer
			outside := filepath.Join(install.home, "outside")
			integrationTestWrite(t, filepath.Join(outside, "keep"), []byte("private external state"))
			switch kind {
			case "state-symlink", "config-symlink":
				path := install.state
				if kind == "config-symlink" {
					path = install.config
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "lock-symlink":
				if err := os.MkdirAll(install.state, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "keep"), filepath.Join(install.state, "install.lock")); err != nil {
					t.Fatal(err)
				}
			case "config-writable", "marker-writable":
				filename := filepath.Join(install.config, "pueue.yml")
				raw := mustMetadataJSON(t, install.freshConfig())
				if kind == "marker-writable" {
					filename = filepath.Join(install.root, "build-info.json")
					raw = mustMetadataJSON(t, metadataTestInfo(t, "123"))
				}
				integrationTestWrite(t, filename, raw)
				if err := os.Chmod(filename, 0666); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.run(t); err == nil {
				t.Fatal("unsafe private path accepted")
			}
			if string(integrationTestRead(t, filepath.Join(outside, "keep"))) != "private external state" {
				t.Fatal("unsafe private path changed external state")
			}
		})
	}
}

func TestInstallerBundleRootOwnershipDoesNotRelaxPrivateReads(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("requires a non-root account to distinguish bundle and private ownership")
	}
	// Read a real immutable root-owned resource without requiring privileged
	// fixture writes. Exercise exactly the same bundle reader used during setup.
	filename := "/usr/bin/true"
	var info unix.Stat_t
	if err := unix.Stat(filename, &info); err != nil {
		t.Fatal(err)
	}
	if info.Uid != 0 || info.Nlink != 1 {
		t.Skip("system fixture is not a single-link root-owned resource")
	}
	if _, err := installReadPayload("/usr", filename, updateMaxArchive, true); err != nil {
		t.Fatalf("immutable root-owned bundle resource refused: %v", err)
	}
	if _, err := installReadPayload("/usr", filename, updateMaxArchive, false); err == nil {
		t.Fatal("classic payload accepted a foreign-owned file")
	}
	if _, err := updateReadOwned(filename, updateMaxArchive); err == nil {
		t.Fatal("private metadata reader accepted a root-owned file")
	}
	if err := installValidateAppPath(integrationHome(), "/Applications/Agent Queue.app"); err != nil {
		t.Fatalf("system Applications location refused: %v", err)
	}
}

func TestInstallerClassicPayloadCanonicalBoundary(t *testing.T) {
	f := installTestFixture(t)
	alias := filepath.Join(filepath.Dir(f.installer.payload), "payload-alias")
	if err := os.Symlink(f.installer.payload, alias); err != nil {
		t.Fatal(err)
	}
	f.installer.payload = alias
	if err := f.run(t); err != nil {
		t.Fatalf("classic installer source alias no longer supported: %v", err)
	}
}
