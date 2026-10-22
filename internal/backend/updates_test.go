package backend

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"howett.net/plist"
)

type updateFixture struct {
	root, app, state string
	manifest         Manifest
	release          Object
	archive          []byte
	commands         [][]string
	client           *UpdateClient
}

func updateTestFixture(t *testing.T) *updateFixture {
	t.Helper()
	directory := t.TempDir()
	info := metadataTestInfo(t, "20")
	commit := strings.Repeat("a", 40)
	info.Commit = &commit
	f := &updateFixture{root: filepath.Join(directory, "installed"), app: filepath.Join(directory, "Agent Queue.app"), state: filepath.Join(directory, "state"), manifest: Manifest{BuildInfo: info, Tag: "build-20-aaaaaaa", Asset: "Agent-Job-Queue-Installer-0.1.0-universal.zip"}}
	if err := os.Mkdir(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	f.archive = f.makeArchive(t, false)
	sum := sha256.Sum256(f.archive)
	f.manifest.SHA256 = hex.EncodeToString(sum[:])
	f.release = Object{"draft": false, "prerelease": true, "tag_name": f.manifest.Tag, "target_commitish": commit}
	f.refreshAssets(t)
	f.client = NewUpdateClient(f.root, f.app, f.state)
	f.client.download = f.download(t)
	f.client.command = func(ctx context.Context, args []string) error {
		f.commands = append(f.commands, append([]string{}, args...))
		if args[0] == "/usr/bin/ditto" {
			archive, err := zip.OpenReader(args[len(args)-2])
			if err != nil {
				return err
			}
			defer archive.Close()
			for _, file := range archive.File {
				destination := filepath.Join(args[len(args)-1], filepath.FromSlash(file.Name))
				if file.FileInfo().IsDir() {
					if err := os.MkdirAll(destination, 0700); err != nil {
						return err
					}
					continue
				}
				if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
					return err
				}
				source, err := file.Open()
				if err != nil {
					return err
				}
				data, err := io.ReadAll(source)
				source.Close()
				if err != nil {
					return err
				}
				if err := os.WriteFile(destination, data, 0600); err != nil {
					return err
				}
			}
		}
		return nil
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

func (f *updateFixture) refreshAssets(t *testing.T) {
	t.Helper()
	assets := []any{}
	for _, item := range []struct {
		name string
		data []byte
	}{{f.manifest.Asset, f.archive}, {"agent-job-queue.json", mustMetadataJSON(t, f.manifest)}} {
		sum := sha256.Sum256(item.data)
		assets = append(assets, Object{"name": item.name, "browser_download_url": f.url(item.name), "digest": "sha256:" + hex.EncodeToString(sum[:])})
	}
	f.release["assets"] = assets
}

func (f *updateFixture) url(name string) string {
	return "https://github.com/" + ReleaseRepository + "/releases/download/" + f.manifest.Tag + "/" + name
}

func (f *updateFixture) download(t *testing.T) func(context.Context, string, int64, string) ([]byte, error) {
	return func(ctx context.Context, url string, limit int64, destination string) ([]byte, error) {
		var data []byte
		switch url {
		case updateAPI:
			data = mustMetadataJSON(t, []any{f.release})
		case f.url("agent-job-queue.json"):
			data = mustMetadataJSON(t, f.manifest)
		case f.url(f.manifest.Asset):
			data = f.archive
		default:
			t.Fatalf("unexpected download %s", url)
		}
		if int64(len(data)) > limit {
			t.Fatal("test exceeds download limit")
		}
		if destination != "" {
			return nil, os.WriteFile(destination, data, 0600)
		}
		return data, nil
	}
}

func (f *updateFixture) makeArchive(t *testing.T, mismatch bool) []byte {
	t.Helper()
	directory := t.TempDir()
	installer := filepath.Join(directory, updateInstaller)
	updateTestBundle(t, installer, "io.went.agent-queue.installer", f.manifest.BuildInfo, plist.BinaryFormat)
	payload := filepath.Join(installer, "Contents/Resources/payload")
	updateTestBundle(t, filepath.Join(payload, "Agent Queue.app"), "io.went.agent-queue", f.manifest.BuildInfo, plist.XMLFormat)
	info := f.manifest.BuildInfo
	if mismatch {
		build := 19
		info.Build = &build
	}
	integrationTestWrite(t, filepath.Join(payload, "source/build-info.json"), mustMetadataJSON(t, info))
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	err := filepath.WalkDir(directory, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(directory, filename)
		if err != nil {
			return err
		}
		writer, err := archive.Create(filepath.ToSlash(relative))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		_, err = writer.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestUpdatesInstalledVersionOfflineAndLegacy(t *testing.T) {
	f := updateTestFixture(t)
	result, err := updateInstalled(f.root, f.app)
	if err != nil || result["displayVersion"] != "Not installed" {
		t.Fatalf("missing install %v %v", result, err)
	}
	data, err := plist.Marshal(map[string]any{"CFBundleIdentifier": "io.went.agent-queue", "CFBundleShortVersionString": "1.0", "CFBundleVersion": "1"}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(f.app, "Contents/Info.plist"), data)
	result, err = updateInstalled(f.root, f.app)
	if err != nil || result["build"] != nil || result["tracked"] != false || !strings.Contains(result["displayVersion"].(string), "legacy") {
		t.Fatalf("legacy %v %v", result, err)
	}
	if _, err := os.Stat(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("version created network/update state")
	}
	f.install(t, 0, "")
	result, err = updateInstalled(f.root, f.app)
	if err != nil || result["build"] != nil || result["bundleBuild"] != "0" {
		t.Fatalf("untracked %v %v", result, err)
	}
}

func TestUpdatesInstalledVersionUsesRegisteredApp(t *testing.T) {
	f := installTestFixture(t)
	f.appBundle(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	install := f.installer
	app := filepath.Join(install.home, "Applications/Registered Agent Queue.app")
	if err := os.Rename(install.app, app); err != nil {
		t.Fatal(err)
	}
	install.app = app
	if err := install.serviceDefinitions(false); err != nil {
		t.Fatal(err)
	}
	before := installBundleSnapshot(t, app)
	result, err := updateInstalled("", "")
	if err != nil || result["build"] != 123 {
		t.Fatalf("registered installed version not resolved: %v %v", result, err)
	}
	if code := UpdatesMain([]string{"version", "--json"}); code != 0 {
		t.Fatalf("version CLI cannot resolve the registered bundle: %d", code)
	}
	// A default update client must resolve the same bundle before fetching releases.
	client := NewUpdateClient("", "", "")
	client.download = func(context.Context, string, int64, string) ([]byte, error) { return []byte("[]"), nil }
	status, err := client.Check()
	if err != nil || obj(status["installed"])["build"] != 123 {
		t.Fatalf("update client cannot resolve registered version: %v %v", status, err)
	}
	if after := installBundleSnapshot(t, app); !reflect.DeepEqual(before, after) {
		t.Fatal("version resolution mutated the registered app")
	}
}

func TestUpdatesRegisteredAppRejectsUnsafeRegistration(t *testing.T) {
	for _, kind := range []string{"label", "command", "program", "argument", "downloads", "traversal", "symlink-app", "symlink-parent", "symlink-plist", "writable-plist", "hardlink-plist", "invalid-plist", "symlink-marker", "writable-marker", "writable-bundle"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.appBundle(t)
			if err := f.run(t); err != nil {
				t.Fatal(err)
			}
			install := f.installer
			filename := filepath.Join(install.launch, "io.went.agent-queue.plist")
			definition, err := updatePlist(filename)
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
			if result, err := updateInstalled("", ""); err == nil {
				t.Fatalf("unsafe registration or metadata accepted: %v", result)
			}
		})
	}
}

func TestUpdatesDefaultAppLegacyFallback(t *testing.T) {
	f := installTestFixture(t)
	if result, err := updateInstalled("", ""); err != nil || result["displayVersion"] != "Not installed" {
		t.Fatalf("uninstalled default lookup failed: %v %v", result, err)
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.installer.launch, "io.went.agent-queue.plist")); err != nil {
		t.Fatal(err)
	}
	if result, err := updateInstalled("", ""); err != nil || result["build"] != 123 {
		t.Fatalf("legacy default app fallback failed: %v %v", result, err)
	}
}

func TestUpdatesRejectsIncompleteMismatchedAndUnsafeMetadata(t *testing.T) {
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
			if _, err := updateInstalled(f.root, f.app); err == nil {
				t.Fatal("unsafe installed identity accepted")
			}
		})
	}
}

func TestUpdatesComparisonUsesImmutableBuilds(t *testing.T) {
	f := updateTestFixture(t)
	for _, test := range []struct {
		build              int
		commit, comparison string
		install            bool
	}{{10, strings.Repeat("b", 40), "available", true}, {20, strings.Repeat("a", 40), "current", false}, {21, strings.Repeat("c", 40), "newer", false}, {0, "", "untracked", true}} {
		f.install(t, test.build, test.commit)
		result, err := f.client.Check()
		if err != nil || result["comparison"] != test.comparison || result["canInstall"] != test.install {
			t.Fatalf("comparison %+v: %v %v", test, result, err)
		}
	}
	f.install(t, 20, strings.Repeat("b", 40))
	if _, err := f.client.Check(); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("reused build: %v", err)
	}
	f.client.download = func(context.Context, string, int64, string) ([]byte, error) { return []byte("[]"), nil }
	result, err := f.client.Check()
	if err != nil || result["comparison"] != "unavailable" {
		t.Fatalf("missing release %v %v", result, err)
	}
	if len(f.commands) != 0 {
		t.Fatal("check ran installer commands")
	}
	if _, err := os.Stat(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("check created storage")
	}
}

func TestUpdatesReleasePaginationAndTrust(t *testing.T) {
	for _, kind := range []string{"paginate", "truncated", "duplicate-build", "draft", "commit", "asset-url", "duplicate-asset", "manifest-digest", "invalid-json", "null-json"} {
		t.Run(kind, func(t *testing.T) {
			f := updateTestFixture(t)
			download := f.client.download
			switch kind {
			case "commit":
				f.release["target_commitish"] = strings.Repeat("b", 40)
			case "asset-url":
				f.release["assets"].([]any)[0].(Object)["browser_download_url"] = "https://example.invalid/file"
			case "duplicate-asset":
				assets := f.release["assets"].([]any)
				f.release["assets"] = append(assets, assets[1])
			case "manifest-digest":
				f.release["assets"].([]any)[1].(Object)["digest"] = "sha256:" + strings.Repeat("0", 64)
			case "draft":
				f.release["draft"] = true
			}
			f.client.pages = 2
			f.client.download = func(ctx context.Context, url string, limit int64, destination string) ([]byte, error) {
				if strings.HasPrefix(url, updateAPI) {
					switch kind {
					case "paginate":
						if url == updateAPI {
							unrelated := make([]any, 100)
							for index := range unrelated {
								unrelated[index] = Object{"tag_name": "unrelated", "draft": false}
							}
							return mustMetadataJSON(t, unrelated), nil
						}
						return mustMetadataJSON(t, []any{f.release}), nil
					case "truncated":
						items := make([]any, 100)
						for index := range items {
							items[index] = f.release
						}
						return mustMetadataJSON(t, items), nil
					case "duplicate-build":
						return mustMetadataJSON(t, []any{f.release, f.release}), nil
					case "invalid-json":
						return []byte("{}"), nil
					case "null-json":
						return []byte(" null \n"), nil
					}
				}
				return download(ctx, url, limit, destination)
			}
			result, err := f.client.Check()
			if kind == "paginate" {
				if err != nil || result["latest"].(Object)["build"] != 20 {
					t.Fatalf("pagination %v %v", result, err)
				}
			} else if kind == "draft" {
				if err != nil || result["latest"] != nil {
					t.Fatalf("draft %v %v", result, err)
				}
			} else if err == nil {
				t.Fatalf("unsafe release accepted: %s %v", kind, result)
			}
		})
	}
}

func TestUpdatesInstallerHandoffAndBoundedStorage(t *testing.T) {
	f := updateTestFixture(t)
	f.install(t, 10, strings.Repeat("b", 40))
	for index := 0; index < 3; index++ {
		result, err := f.client.OpenInstaller(20)
		if err != nil || result["installerOpened"] != true {
			t.Fatalf("prepare %v %v", result, err)
		}
	}
	if _, err := f.client.OpenInstaller(20); err == nil || !strings.Contains(err.Error(), "previously opened") {
		t.Fatalf("unbounded storage: %v", err)
	}
	var commands []string
	for _, args := range f.commands[:4] {
		commands = append(commands, args[0])
	}
	if strings.Join(commands, ",") != "/usr/bin/ditto,/usr/bin/codesign,/usr/bin/codesign,/usr/bin/open" || f.commands[3][1] != "-n" {
		t.Fatalf("unsafe handoff: %v", f.commands)
	}
	installed, err := updateInstalled(f.root, f.app)
	if err != nil || installed["build"] != 10 {
		t.Fatal("handoff installed without confirmation")
	}
	ready, _ := filepath.Glob(filepath.Join(f.state, "updates/ready-*"))
	if len(ready) != 3 {
		t.Fatalf("prepared count %d", len(ready))
	}
	// Completed older preparations are removed only after that build is installed.
	f.install(t, 21, strings.Repeat("c", 40))
	for _, directory := range ready {
		if _, err := os.Stat(filepath.Join(directory, "release.json")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdatesFailuresNeverOpenOrLeakPreparation(t *testing.T) {
	for _, kind := range []string{"stale", "current", "newer", "checksum", "identity", "signature", "changed-install", "open-failure"} {
		t.Run(kind, func(t *testing.T) {
			f := updateTestFixture(t)
			f.install(t, 10, strings.Repeat("b", 40))
			requested := 20
			switch kind {
			case "stale":
				requested = 19
			case "current":
				f.install(t, 20, strings.Repeat("a", 40))
			case "newer":
				f.install(t, 21, strings.Repeat("c", 40))
			case "checksum":
				f.archive = append(f.archive, []byte("tampered")...)
			case "identity":
				f.archive = f.makeArchive(t, true)
				sum := sha256.Sum256(f.archive)
				f.manifest.SHA256 = hex.EncodeToString(sum[:])
				f.refreshAssets(t)
			}
			command := f.client.command
			f.client.command = func(ctx context.Context, args []string) error {
				if kind == "signature" && args[0] == "/usr/bin/codesign" {
					return errors.New("Invalid signature")
				}
				if kind == "open-failure" && args[0] == "/usr/bin/open" {
					return errors.New("Open failed")
				}
				if err := command(ctx, args); err != nil {
					return err
				}
				if kind == "changed-install" && args[0] == "/usr/bin/codesign" {
					f.install(t, 21, strings.Repeat("c", 40))
				}
				return nil
			}
			if _, err := f.client.OpenInstaller(requested); err == nil {
				t.Fatal("unsafe installer opened")
			}
			for _, args := range f.commands {
				if args[0] == "/usr/bin/open" {
					t.Fatal("failure opened installer")
				}
			}
			entries, err := os.ReadDir(filepath.Join(f.state, "updates"))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("stage leaked: %v", entries)
			}
		})
	}
}

func TestUpdatesArchiveRejectsUnsafeEntries(t *testing.T) {
	for _, test := range []struct {
		names []string
		mode  os.FileMode
	}{
		{[]string{"../outside"}, 0600}, {[]string{"/absolute"}, 0600}, {[]string{updateInstaller + "/link"}, os.ModeSymlink | 0600}, {[]string{updateInstaller + "/pipe"}, os.ModeNamedPipe | 0600},
		{[]string{updateInstaller + "/../outside"}, 0600}, {[]string{updateInstaller + "/./file"}, 0600}, {[]string{updateInstaller + "/FILE", updateInstaller + "/file"}, 0600},
		{[]string{updateInstaller + "/caf\u00e9", updateInstaller + "/cafe\u0301"}, 0600}, {[]string{updateInstaller + "/STRASSE", updateInstaller + "/Stra\u00dfe"}, 0600},
		{[]string{updateInstaller + "/evil\x00ignored"}, 0600}, {[]string{updateInstaller + "/a:b"}, 0600}, {[]string{updateInstaller + "/a\\b"}, 0600},
	} {
		t.Run(fmt.Sprint(test.names), func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "bad.zip")
			file, err := os.Create(filename)
			if err != nil {
				t.Fatal(err)
			}
			archive := zip.NewWriter(file)
			for _, name := range test.names {
				header := &zip.FileHeader{Name: name}
				header.SetMode(test.mode)
				writer, err := archive.CreateHeader(header)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = writer.Write([]byte("data"))
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			file.Close()
			if err := updateInspectArchive(filename); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestUpdatesLockRefusesConcurrentAndUnsafeFiles(t *testing.T) {
	f := updateTestFixture(t)
	lock, err := updateLock(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := f.client.OpenInstaller(20); err == nil || !strings.Contains(err.Error(), "Another update") {
		t.Fatalf("concurrent update %v", err)
	}
	lock.Close()
	lockPath := filepath.Join(f.state, "updates.lock")
	if err := os.Chmod(lockPath, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := updateLock(f.state); err == nil {
		t.Fatal("writable lock accepted")
	}
}

type updateRoundTrip func(*http.Request) (*http.Response, error)

func (roundTrip updateRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestUpdatesTransportRejectsURLsAndBoundsResponse(t *testing.T) {
	for _, raw := range []string{"http://github.com/file", "https://evil.invalid/file", "https://github.com:8080/file", "https://person@github.com/file", "file:///installer", "https://github.com/file#fragment", "https://github.com/file#", "https://github.com:bad/file"} {
		if updateTrustedURL(raw) {
			t.Errorf("trusted unsafe URL %s", raw)
		}
	}
	for _, test := range []struct {
		body, declared string
		status         int
	}{{"large", "5", 200}, {"large", "", 200}, {"x", "2", 200}, {"x", "invalid", 200}, {"", "", 429}, {"", "", 500}} {
		client := &http.Client{Transport: updateRoundTrip(func(request *http.Request) (*http.Response, error) {
			headers := http.Header{}
			if test.declared != "" {
				headers.Set("Content-Length", test.declared)
			}
			return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)), Header: headers, ContentLength: -1, Request: request}, nil
		})}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := updateDownload(ctx, client, updateAPI, 3, "")
		cancel()
		if err == nil {
			t.Fatalf("unbounded/truncated response accepted: %+v", test)
		}
	}
}

func TestUpdatesAbsoluteDeadlineInterruptsHeadersAndTricklingBody(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if phase == "headers" {
					<-request.Context().Done()
					return
				}
				writer.Header().Set("Content-Length", "200")
				writer.WriteHeader(200)
				writer.(http.Flusher).Flush()
				for {
					select {
					case <-request.Context().Done():
						return
					case <-time.After(10 * time.Millisecond):
						_, _ = writer.Write([]byte{'x'})
						writer.(http.Flusher).Flush()
					}
				}
			}))
			defer server.Close()
			client := server.Client()
			transport := client.Transport.(*http.Transport)
			transport.TLSClientConfig.ServerName = "example.com"
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err := updateDownload(ctx, client, updateAPI, 1024, "")
			if err == nil || !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("hard deadline: %v", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("deadline did not bound trickling I/O")
			}
		})
	}
}
