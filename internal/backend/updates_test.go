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
	root, app, state, downloads string
	manifest                    Manifest
	release                     Object
	archive                     []byte
	commands                    [][]string
	client                      *UpdateClient
}

func updateTestFixture(t *testing.T) *updateFixture {
	t.Helper()
	directory := t.TempDir()
	info := metadataTestInfo(t, "20")
	commit := strings.Repeat("a", 40)
	info.Commit = &commit
	f := &updateFixture{root: filepath.Join(directory, "installed"), app: filepath.Join(directory, "Agent Queue.app"), state: filepath.Join(directory, "state"), downloads: filepath.Join(directory, "Downloads"), manifest: Manifest{BuildInfo: info, Tag: "build-20-aaaaaaa", Asset: "Agent-Queue-0.1.0-universal.zip"}}
	if err := os.Mkdir(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.downloads, 0700); err != nil {
		t.Fatal(err)
	}
	f.archive = f.makeArchive(t, false)
	sum := sha256.Sum256(f.archive)
	f.manifest.SHA256 = hex.EncodeToString(sum[:])
	f.release = Object{"draft": false, "prerelease": true, "tag_name": f.manifest.Tag, "target_commitish": commit}
	f.refreshAssets(t)
	f.client = NewUpdateClient(f.root, f.app, f.state)
	f.client.download = f.download(t)
	f.client.Downloads = f.downloads
	f.client.command = func(ctx context.Context, args []string) error {
		f.commands = append(f.commands, append([]string{}, args...))
		if args[0] == "/usr/bin/ditto" && args[1] != "-x" {
			return updateTestCopy(args[1], args[2])
		}
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

func updateTestCopy(source, destination string) error {
	return filepath.WalkDir(source, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, filename)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	})
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
	app := filepath.Join(directory, updateApp)
	updateTestBundle(t, app, "io.went.agent-queue", f.manifest.BuildInfo, plist.BinaryFormat)
	info := f.manifest.BuildInfo
	if mismatch {
		build := 19
		info.Build = &build
	}
	integrationTestWrite(t, filepath.Join(app, "Contents/Resources/payload/source/build-info.json"), mustMetadataJSON(t, info))
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
		if err != nil || result["comparison"] != test.comparison || result["canDownload"] != test.install {
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

func TestUpdatesDownloadDeliversVerifiedAppAndNeverOverwrites(t *testing.T) {
	f := updateTestFixture(t)
	f.install(t, 10, strings.Repeat("b", 40))
	// An earlier folder with the expected name holds user data that must survive.
	occupied := filepath.Join(f.downloads, "Agent Queue 0.1.0 (build 20)")
	integrationTestWrite(t, filepath.Join(occupied, "keep"), []byte("user file"))
	result, err := f.client.DownloadUpdate(20)
	delivered := filepath.Join(f.downloads, "Agent Queue 0.1.0 (build 20) 2", "Agent Queue.app")
	if err != nil || result["downloaded"] != true || result["build"] != 20 || result["app"] != delivered || !strings.Contains(result["message"].(string), "drag the new copy into Applications") {
		t.Fatalf("download %v %v", result, err)
	}
	if string(integrationTestRead(t, filepath.Join(occupied, "keep"))) != "user file" {
		t.Fatal("existing download folder was changed")
	}
	if _, err := os.Stat(filepath.Join(occupied, "Agent Queue.app")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("download wrote into an existing folder")
	}
	var commands []string
	for _, args := range f.commands {
		commands = append(commands, args[0])
	}
	if strings.Join(commands, ",") != "/usr/bin/ditto,/usr/bin/codesign,/usr/bin/ditto,/usr/bin/codesign,/usr/bin/open" || !strings.HasPrefix(f.commands[2][1], filepath.Join(f.state, "updates")) || f.commands[2][2] != delivered || f.commands[4][1] != "-R" || f.commands[4][2] != delivered {
		t.Fatalf("unsafe delivery: %v", f.commands)
	}
	if _, err := f.client.verifyPackage(context.Background(), filepath.Dir(delivered), f.manifest); err != nil {
		t.Fatalf("delivered app is not the verified build: %v", err)
	}
	installed, err := updateInstalled(f.root, f.app)
	if err != nil || installed["build"] != 10 {
		t.Fatal("download changed the installed app")
	}
	// Each further download claims its own folder, and private staging is always emptied.
	for _, suffix := range []string{"3", "4"} {
		if _, err := f.client.DownloadUpdate(20); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(f.downloads, "Agent Queue 0.1.0 (build 20) "+suffix, "Agent Queue.app")); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(f.state, "updates"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging retained: %v %v", entries, err)
	}
}

func TestUpdatesDownloadClearsStaleStagingAndSurvivesRevealFailure(t *testing.T) {
	f := updateTestFixture(t)
	f.install(t, 10, strings.Repeat("b", 40))
	for _, name := range []string{"preparing-old", "ready-old"} {
		if err := os.MkdirAll(filepath.Join(f.state, "updates", name), 0700); err != nil {
			t.Fatal(err)
		}
		integrationTestWrite(t, filepath.Join(f.state, "updates", name, "release.json"), []byte("{}"))
	}
	command := f.client.command
	f.client.command = func(ctx context.Context, args []string) error {
		if args[0] == "/usr/bin/open" {
			return errors.New("Finder unavailable")
		}
		return command(ctx, args)
	}
	result, err := f.client.DownloadUpdate(20)
	app := filepath.Join(f.downloads, "Agent Queue 0.1.0 (build 20)", "Agent Queue.app")
	if err != nil || result["downloaded"] != true || !strings.Contains(result["message"].(string), app) {
		t.Fatalf("reveal failure %v %v", result, err)
	}
	if _, err := os.Stat(app); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(f.state, "updates"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("stale staging retained: %v %v", entries, err)
	}
}

func TestUpdatesFailuresNeverDeliverOrLeakPreparation(t *testing.T) {
	for _, kind := range []string{"stale", "current", "newer", "checksum", "identity", "signature", "copied-signature", "changed-install", "copy-failure", "missing-downloads"} {
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
			case "missing-downloads":
				if err := os.Remove(f.downloads); err != nil {
					t.Fatal(err)
				}
			}
			command := f.client.command
			signatures := 0
			f.client.command = func(ctx context.Context, args []string) error {
				if args[0] == "/usr/bin/codesign" {
					signatures++
					if kind == "signature" || (kind == "copied-signature" && signatures == 2) {
						return errors.New("Invalid signature")
					}
				}
				if kind == "copy-failure" && args[0] == "/usr/bin/ditto" && args[1] != "-x" {
					return errors.New("Copy failed")
				}
				if err := command(ctx, args); err != nil {
					return err
				}
				if kind == "changed-install" && args[0] == "/usr/bin/codesign" {
					f.install(t, 21, strings.Repeat("c", 40))
				}
				return nil
			}
			if _, err := f.client.DownloadUpdate(requested); err == nil {
				t.Fatal("unsafe update delivered")
			}
			for _, args := range f.commands {
				if args[0] == "/usr/bin/open" {
					t.Fatal("failure revealed an update")
				}
			}
			if kind != "missing-downloads" {
				delivered, err := os.ReadDir(f.downloads)
				if err != nil || len(delivered) != 0 {
					t.Fatalf("download leaked: %v %v", delivered, err)
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
		{[]string{"../outside"}, 0600}, {[]string{"/absolute"}, 0600}, {[]string{"Agent Job Queue Installer.app/file"}, 0600}, {[]string{"Agent Queue.app/file", "Other.app/file"}, 0600}, {[]string{updateApp + "/link"}, os.ModeSymlink | 0600}, {[]string{updateApp + "/pipe"}, os.ModeNamedPipe | 0600},
		{[]string{updateApp + "/../outside"}, 0600}, {[]string{updateApp + "/./file"}, 0600}, {[]string{updateApp + "/FILE", updateApp + "/file"}, 0600},
		{[]string{updateApp + "/caf\u00e9", updateApp + "/cafe\u0301"}, 0600}, {[]string{updateApp + "/STRASSE", updateApp + "/Stra\u00dfe"}, 0600},
		{[]string{updateApp + "/evil\x00ignored"}, 0600}, {[]string{updateApp + "/a:b"}, 0600}, {[]string{updateApp + "/a\\b"}, 0600},
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
	if _, err := f.client.DownloadUpdate(20); err == nil || !strings.Contains(err.Error(), "Another update") {
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

func TestUpdatesCLIUsesDownloadAction(t *testing.T) {
	for _, args := range [][]string{{"updates", "install", "--build", "20"}, {"updates", "download"}, {"updates", "check", "--build", "20"}, {"updates", "download", "--build", "x"}} {
		if code := UpdatesMain(args); code != 2 {
			t.Errorf("%v returned %d", args, code)
		}
	}
}
