package backend

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"howett.net/plist"
)

type updateFixture struct {
	root, app, state string
	manifest         Manifest
}

func updateTestFixture(t *testing.T) *updateFixture {
	t.Helper()
	directory := t.TempDir()
	info := metadataTestInfo(t, "20")
	commit := strings.Repeat("a", 40)
	info.Commit = &commit
	f := &updateFixture{root: filepath.Join(directory, "installed"), app: filepath.Join(directory, "Agent Queue.app"), state: filepath.Join(directory, "state"), manifest: Manifest{BuildInfo: info, Tag: "build-20-aaaaaaa", Asset: "Agent-Queue-0.1.0-universal.zip"}}
	if err := os.Mkdir(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	return f
}

func updateTestBundle(t *testing.T, directory, identifier string, info BuildInfo, format int) {
	t.Helper()
	resources := filepath.Join(directory, "Contents/Resources")
	if err := os.MkdirAll(resources, 0700); err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(resources, "build-info.json"), mustMetadataJSON(t, info))
	build := "0"
	if info.Build != nil {
		build = strconv.Itoa(*info.Build)
	}
	data, err := plist.Marshal(map[string]any{"CFBundleIdentifier": identifier, "CFBundleShortVersionString": info.Version, "CFBundleVersion": build}, format)
	if err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(directory, "Contents/Info.plist"), data)
}

func (f *updateFixture) install(t *testing.T, build int, commit string) {
	t.Helper()
	info := f.manifest.BuildInfo
	if build == 0 {
		info.Build = nil
		info.Commit = nil
	} else {
		info.Build = &build
		info.Commit = &commit
	}
	integrationTestWrite(t, filepath.Join(f.root, "build-info.json"), mustMetadataJSON(t, info))
	updateTestBundle(t, f.app, "io.went.agent-queue", info, plist.XMLFormat)
}

func TestVersionInstalledVersionOfflineAndLegacy(t *testing.T) {
	f := updateTestFixture(t)
	result, err := installedVersionReport(f.root, f.app)
	if err != nil || result["displayVersion"] != "Not installed" {
		t.Fatalf("missing install %v %v", result, err)
	}
	data, err := plist.Marshal(map[string]any{"CFBundleIdentifier": "io.went.agent-queue", "CFBundleShortVersionString": "1.0", "CFBundleVersion": "1"}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(f.app, "Contents/Info.plist"), data)
	result, err = installedVersionReport(f.root, f.app)
	if err != nil || result["build"] != nil || result["tracked"] != false || !strings.Contains(result["displayVersion"].(string), "legacy") {
		t.Fatalf("legacy %v %v", result, err)
	}
	if _, err := os.Stat(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("version created network/update state")
	}
	f.install(t, 0, "")
	result, err = installedVersionReport(f.root, f.app)
	if err != nil || result["build"] != nil || result["bundleBuild"] != "0" {
		t.Fatalf("untracked %v %v", result, err)
	}
}

func TestVersionInstalledVersionUsesRegisteredApp(t *testing.T) {
	f := installTestFixture(t)
	install := f.installer
	installOlderUnmarkedState(t, install)
	app := filepath.Join(install.home, "Applications/Registered Agent Queue.app")
	if err := os.Rename(install.app, app); err != nil {
		t.Fatal(err)
	}
	install.app = app
	installOlderServiceDefinitions(t, install)
	before := installBundleSnapshot(t, app)
	result, err := installedVersionReport("", "")
	if err != nil || result["build"] != 123 {
		t.Fatalf("registered installed version not resolved: %v %v", result, err)
	}
	if code := VersionMain([]string{"version", "--json"}); code != 0 {
		t.Fatalf("version CLI cannot resolve the registered bundle: %d", code)
	}
	if after := installBundleSnapshot(t, app); !reflect.DeepEqual(before, after) {
		t.Fatal("version resolution mutated the registered app")
	}
}

func TestVersionRegisteredAppRejectsUnsafeRegistration(t *testing.T) {
	for _, kind := range []string{"label", "command", "program", "argument", "downloads", "traversal", "symlink-app", "symlink-parent", "symlink-plist", "writable-plist", "hardlink-plist", "invalid-plist", "symlink-marker", "writable-marker", "writable-bundle"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			install := f.installer
			installOlderUnmarkedState(t, install)
			filename := filepath.Join(install.launch, "io.went.agent-queue.plist")
			definition, err := readBundlePlist(filename)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "label", "command", "program", "argument", "downloads", "traversal":
				args := []string{"/usr/bin/open", "-a", install.app}
				switch kind {
				case "label":
					definition["Label"] = "operator.service"
				case "command":
					args[0] = "/operator/open"
				case "program":
					definition["Program"] = "/operator/open"
				case "argument":
					args = append(args, "--unexpected")
				case "downloads":
					args[2] = filepath.Join(install.home, "Downloads/Agent Queue.app")
				case "traversal":
					args[2] = install.home + "/Downloads/../Applications/Agent Queue.app"
				}
				definition["ProgramArguments"] = args
				if err := install.writePlist(filename, definition); err != nil {
					t.Fatal(err)
				}
			case "symlink-app", "symlink-parent", "symlink-plist", "symlink-marker", "hardlink-plist":
				target := filename
				switch kind {
				case "symlink-app":
					target = install.app
				case "symlink-parent":
					target = install.launch
				case "symlink-marker":
					target = filepath.Join(install.root, "build-info.json")
				}
				outside := filepath.Join(install.home, "outside")
				if err := os.Rename(target, outside); err != nil {
					t.Fatal(err)
				}
				link := os.Symlink
				if kind == "hardlink-plist" {
					link = os.Link
				}
				if err := link(outside, target); err != nil {
					t.Fatal(err)
				}
			case "writable-plist", "writable-marker", "writable-bundle":
				if kind == "writable-marker" {
					filename = filepath.Join(install.root, "build-info.json")
				} else if kind == "writable-bundle" {
					filename = filepath.Join(install.app, "Contents/Resources/build-info.json")
				}
				if err := os.Chmod(filename, 0666); err != nil {
					t.Fatal(err)
				}
			case "invalid-plist":
				integrationTestWrite(t, filename, []byte("not a plist"))
			}
			if result, err := installedVersionReport("", ""); err == nil {
				t.Fatalf("unsafe registration or metadata accepted: %v", result)
			}
		})
	}
}

func TestVersionDefaultAppLegacyFallback(t *testing.T) {
	f := installTestFixture(t)
	aside := filepath.Join(f.installer.home, "Agent Queue aside.app")
	if err := os.Rename(f.installer.app, aside); err != nil {
		t.Fatal(err)
	}
	if result, err := installedVersionReport("", ""); err != nil || result["displayVersion"] != "Not installed" {
		t.Fatalf("uninstalled default lookup failed: %v %v", result, err)
	}
	if err := os.Rename(aside, f.installer.app); err != nil {
		t.Fatal(err)
	}
	installOlderUnmarkedState(t, f.installer)
	if err := os.Remove(filepath.Join(f.installer.launch, "io.went.agent-queue.plist")); err != nil {
		t.Fatal(err)
	}
	if result, err := installedVersionReport("", ""); err != nil || result["build"] != 123 {
		t.Fatalf("legacy default app fallback failed: %v %v", result, err)
	}
}

func TestVersionRejectsIncompleteMismatchedAndUnsafeMetadata(t *testing.T) {
	for _, kind := range []string{"pending", "corrupt", "missing-app", "mismatch", "symlink", "hardlink", "writable", "fifo", "plist"} {
		t.Run(kind, func(t *testing.T) {
			f := updateTestFixture(t)
			f.install(t, 10, strings.Repeat("b", 40))
			marker := filepath.Join(f.root, "build-info.json")
			switch kind {
			case "pending":
				integrationTestWrite(t, filepath.Join(f.root, "install-in-progress.json"), mustMetadataJSON(t, f.manifest.BuildInfo))
			case "corrupt":
				integrationTestWrite(t, marker, []byte("{invalid"))
			case "missing-app":
				if err := os.Remove(filepath.Join(f.app, "Contents/Resources/build-info.json")); err != nil {
					t.Fatal(err)
				}
			case "mismatch":
				integrationTestWrite(t, filepath.Join(f.app, "Contents/Resources/build-info.json"), mustMetadataJSON(t, f.manifest.BuildInfo))
			case "symlink", "hardlink":
				other := marker + ".other"
				if err := os.Rename(marker, other); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "symlink" {
					err = os.Symlink(other, marker)
				} else {
					err = os.Link(other, marker)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(marker, 0666); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(marker, 0600); err != nil {
					t.Fatal(err)
				}
			case "plist":
				integrationTestWrite(t, filepath.Join(f.app, "Contents/Info.plist"), []byte("invalid plist"))
			}
			if _, err := installedVersionReport(f.root, f.app); err == nil {
				t.Fatal("unsafe installed identity accepted")
			}
		})
	}
}

func TestVersionCLIRejectsRemovedUpdatesCommand(t *testing.T) {
	if code := Main([]string{"updates", "check"}); code == 0 {
		t.Fatal("the removed updates command was accepted")
	}
	if code := VersionMain([]string{"version", "--build", "1"}); code != 2 {
		t.Fatalf("version accepted an unknown argument: %d", code)
	}
}
