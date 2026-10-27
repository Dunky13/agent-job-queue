package backend

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"howett.net/plist"
)

const updateAPI = "https://api.github.com/repos/" + ReleaseRepository + "/releases?per_page=100"
const updateInstaller = "Agent Job Queue Installer.app"
const updateMaxArchive = 128 * 1024 * 1024
const updateMaxExpanded = 512 * 1024 * 1024
const updateMaxPages = 10

var updateTag = regexp.MustCompile(`^build-([1-9][0-9]{0,8})-([0-9a-f]{7})$`)
var updateDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var updateDecimal = regexp.MustCompile(`^[0-9]+$`)

func updateReadOwned(path string, limit int64) ([]byte, error) {
	return appservices.ReadFile(filepath.Dir(path), path, limit, false)
}

func updateExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func updateCompleted(root string) error {
	pending := filepath.Join(root, "install-in-progress.json")
	exists, err := updateExists(pending)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	data, err := updateReadOwned(pending, 16384)
	if err != nil {
		return err
	}
	if _, err := ValidateBuildInfo(data); err != nil {
		return err
	}
	return errors.New("Installation is incomplete. Finish or retry the installer before checking versions or updates")
}

func updatePlist(path string) (map[string]any, error) {
	data, err := updateReadOwned(path, 16384)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if _, err := plist.Unmarshal(data, &value); err != nil {
		return nil, errors.New("Installed version metadata is invalid")
	}
	return value, nil
}

func updatePrintable(value any) bool {
	text, ok := value.(string)
	if !ok || len([]rune(text)) < 1 || len([]rune(text)) > 80 {
		return false
	}
	for _, r := range text {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func updateInstalled(root, app string) (Object, error) {
	if root == "" {
		root = filepath.Join(integrationHome(), ".local/share/agent-heavy")
	}
	if app == "" {
		var err error
		app, err = updateRegisteredApp()
		if err != nil {
			return nil, err
		}
	}
	if err := updateCompleted(root); err != nil {
		return nil, err
	}
	result, err := updateInstalledIdentity(root, app)
	if err != nil {
		return nil, err
	}
	if err := updateCompleted(root); err != nil {
		return nil, err
	}
	return result, nil
}

func updateRegisteredApp() (string, error) {
	home := integrationHome()
	if managed, err := appservices.Load(home); err != nil {
		return "", err
	} else if managed != nil {
		if err := updateCompleted(filepath.Join(home, ".local/share/agent-heavy")); err != nil {
			return "", err
		}
		if err := appservices.ValidateApp(home, managed.App); err != nil {
			return "", err
		}
		return managed.App, nil
	}
	fallback := filepath.Join(home, "Applications/Agent Queue.app")
	filename := filepath.Join(home, "Library/LaunchAgents/io.went.agent-queue.plist")
	parent, err := integrationParent(filename, false)
	if errors.Is(err, os.ErrNotExist) {
		return fallback, nil
	}
	if err != nil {
		return "", err
	}
	defer parent.Close()
	raw, identity, err := integrationRead(parent, filepath.Base(filename))
	if err != nil {
		return "", err
	}
	if identity == nil {
		return fallback, nil
	}
	var definition Object
	if len(raw) > 16384 {
		return "", errors.New("App launch agent exceeds its size limit")
	}
	if _, err := plist.Unmarshal(raw, &definition); err != nil || definition["Label"] != "io.went.agent-queue" {
		return "", errors.New("App launch agent has invalid registration metadata")
	}
	args, err := stringsFrom(definition["ProgramArguments"])
	if err != nil || len(args) != 3 || args[0] != "/usr/bin/open" || args[1] != "-a" {
		return "", errors.New("App launch agent has an unexpected application command")
	}
	if program, present := definition["Program"]; present && program != args[0] {
		return "", errors.New("App launch agent Program differs from its expected executable")
	}
	if err := installValidateAppPath(home, args[2]); err != nil {
		return "", err
	}
	// This opens but never creates or changes the app directory. It rejects
	// symlinks in the full path, including the Applications parent.
	bundle, err := installPayloadParent(args[2], filepath.Join(args[2], ".version-probe"), true)
	if err != nil {
		return "", err
	}
	if err := bundle.Close(); err != nil {
		return "", err
	}
	return args[2], nil
}

func updateReadApp(app, filename string) ([]byte, error) {
	if installValidateAppPath(integrationHome(), app) == nil {
		return installReadPayload(app, filename, 16384, true)
	}
	// Explicit development and archive paths retain their current-owner policy.
	return updateReadOwned(filename, 16384)
}

func updateAppPlist(app string) (map[string]any, error) {
	raw, err := updateReadApp(app, filepath.Join(app, "Contents/Info.plist"))
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if _, err := plist.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("Installed version metadata is invalid")
	}
	return value, nil
}

func updateInstalledIdentity(root, app string) (Object, error) {
	marker, bundled := filepath.Join(root, "build-info.json"), filepath.Join(app, "Contents/Resources/build-info.json")
	exists, err := updateExists(marker)
	if err != nil {
		return nil, err
	}
	if exists {
		data, err := updateReadOwned(marker, 16384)
		if err != nil {
			return nil, err
		}
		info, err := ValidateBuildInfo(data)
		if err != nil {
			return nil, errors.New("Installed version metadata is invalid")
		}
		data, err = updateReadApp(app, bundled)
		if err != nil {
			return nil, err
		}
		bundle, err := ValidateBuildInfo(data)
		if err != nil {
			return nil, errors.New("Installed version metadata is invalid")
		}
		if !info.Equal(bundle) {
			return nil, errors.New("App and controller versions differ. Complete installation before checking for updates")
		}
		plist, err := updateAppPlist(app)
		if err != nil {
			return nil, err
		}
		buildString := "0"
		var build, commit any
		if info.Build != nil {
			build = *info.Build
			buildString = strconv.Itoa(*info.Build)
		}
		if info.Commit != nil {
			commit = *info.Commit
		}
		if plist["CFBundleIdentifier"] != "io.went.agent-queue" || plist["CFBundleShortVersionString"] != info.Version || plist["CFBundleVersion"] != buildString {
			return nil, errors.New("Installed app version does not match its release metadata")
		}
		display := info.Version + " (untracked build)"
		if info.Build != nil {
			display = fmt.Sprintf("%s (build %d)", info.Version, *info.Build)
		}
		return Object{"version": info.Version, "build": build, "bundleBuild": buildString, "commit": commit, "tracked": info.Build != nil, "displayVersion": display}, nil
	}
	exists, err = updateExists(bundled)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, errors.New("Installed controller version is missing. Complete installation before checking for updates")
	}
	plistPath := filepath.Join(app, "Contents/Info.plist")
	exists, err = updateExists(plistPath)
	if err != nil {
		return nil, err
	}
	if !exists {
		return Object{"version": nil, "build": nil, "bundleBuild": nil, "commit": nil, "tracked": false, "displayVersion": "Not installed"}, nil
	}
	plist, err := updateAppPlist(app)
	if err != nil {
		return nil, err
	}
	version, build := plist["CFBundleShortVersionString"], plist["CFBundleVersion"]
	if plist["CFBundleIdentifier"] != "io.went.agent-queue" || !updatePrintable(version) || !updatePrintable(build) {
		return nil, errors.New("Installed app version is invalid")
	}
	return Object{"version": version, "build": nil, "bundleBuild": build, "commit": nil, "tracked": false, "displayVersion": fmt.Sprintf("%s (build %s, legacy installation)", version, build)}, nil
}

func updateTrustedURL(raw string) bool {
	value, err := url.Parse(raw)
	if err != nil || value.Scheme != "https" || value.User != nil || value.Fragment != "" || strings.Contains(raw, "#") || (value.Port() != "" && value.Port() != "443") {
		return false
	}
	switch value.Hostname() {
	case "api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	}
	return false
}

func updateNetworkError(err error, ctx context.Context) error {
	var timeout net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return errors.New("Update operation timed out. Check your connection and try again")
	}
	return errors.New("Cannot reach the release service. Check your connection and try again")
}

func updateHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 15 * time.Second
	return &http.Client{Transport: transport, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if !updateTrustedURL(request.URL.String()) {
			return errors.New("Release download redirected outside the trusted HTTPS hosts")
		}
		if len(via) >= 10 {
			return errors.New("Release download exceeded its redirect limit")
		}
		return nil
	}}
}

func updateDownload(ctx context.Context, client *http.Client, raw string, limit int64, destination string) ([]byte, error) {
	if !updateTrustedURL(raw) {
		return nil, errors.New("Release download URL is not trusted")
	}
	if ctx.Err() != nil {
		return nil, updateNetworkError(ctx.Err(), ctx)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "Agent-Job-Queue-Updater")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("Accept", "application/octet-stream")
	if request.URL.Hostname() == "api.github.com" {
		request.Header.Set("Accept", "application/vnd.github+json")
	}
	response, err := client.Do(request)
	if err != nil {
		if strings.Contains(err.Error(), "trusted HTTPS hosts") || strings.Contains(err.Error(), "redirect limit") {
			return nil, errors.New("Release download redirected outside the trusted HTTPS hosts")
		}
		return nil, updateNetworkError(err, ctx)
	}
	defer response.Body.Close()
	if response.Request == nil || !updateTrustedURL(response.Request.URL.String()) {
		return nil, errors.New("Release response is not from a trusted HTTPS host")
	}
	if response.StatusCode == 403 || response.StatusCode == 429 {
		return nil, errors.New("GitHub limited the update request. Try again later")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("GitHub could not provide the release. Try again later")
	}
	declared := int64(-1)
	if values, present := response.Header["Content-Length"]; present {
		if len(values) != 1 || !updateDecimal.MatchString(values[0]) {
			return nil, errors.New("Release response exceeds its size limit")
		}
		declared, err = strconv.ParseInt(values[0], 10, 64)
		if err != nil || declared > limit {
			return nil, errors.New("Release response exceeds its size limit")
		}
	} else if response.ContentLength >= 0 {
		declared = response.ContentLength
		if declared > limit {
			return nil, errors.New("Release response exceeds its size limit")
		}
	}
	var output io.Writer
	var file *os.File
	var memory strings.Builder
	if destination != "" {
		file, err = os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		output = file
	} else {
		output = &memory
	}
	size, err := io.Copy(output, io.LimitReader(response.Body, limit+1))
	if ctx.Err() != nil {
		return nil, updateNetworkError(ctx.Err(), ctx)
	}
	if size > limit {
		return nil, errors.New("Release response exceeds its size limit")
	}
	if declared >= 0 && size != declared {
		return nil, errors.New("Release download was incomplete")
	}
	if err != nil {
		return nil, updateNetworkError(err, ctx)
	}
	if file != nil {
		if err := file.Sync(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return []byte(memory.String()), nil
}

func updateAsset(release Object, name string) (Object, error) {
	assets, ok := release["assets"].([]any)
	if !ok {
		return nil, errors.New("Release assets are missing or ambiguous")
	}
	var selected Object
	matches := 0
	for _, value := range assets {
		if asset, ok := value.(map[string]any); ok && asset["name"] == name {
			selected = Object(asset)
			matches++
		}
	}
	if matches != 1 {
		return nil, errors.New("Release assets are missing or ambiguous")
	}
	expected := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", ReleaseRepository, release["tag_name"], name)
	if selected["browser_download_url"] != expected {
		return nil, errors.New("Release asset does not belong to the expected published build")
	}
	if digest := selected["digest"]; digest != nil {
		text, ok := digest.(string)
		if !ok || !updateDigest.MatchString(text) {
			return nil, errors.New("Release asset digest is invalid")
		}
	}
	return selected, nil
}

func updateVerifyDigest(data []byte, asset Object) error {
	if digest := asset["digest"]; digest != nil {
		actual := sha256.Sum256(data)
		if digest != "sha256:"+hex.EncodeToString(actual[:]) {
			return errors.New("Release asset checksum does not match GitHub's digest")
		}
	}
	return nil
}

func updatePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	var info unix.Stat_t
	if err := unix.Lstat(path, &info); err != nil {
		return err
	}
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFDIR || info.Uid != uint32(os.Getuid()) || uint32(info.Mode)&0077 != 0 {
		return errors.New("Update storage has unsafe ownership or permissions")
	}
	return nil
}

func updateLock(state string) (*os.File, error) {
	if err := updatePrivateDirectory(state); err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(state, "updates.lock"), unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "updates.lock")
	fail := func(err error) (*os.File, error) { file.Close(); return nil, err }
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return fail(err)
	}
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Getuid()) || uint32(info.Mode)&0077 != 0 || info.Nlink != 1 {
		return fail(errors.New("Update lock has unsafe ownership or permissions"))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fail(errors.New("Another update is being prepared. Wait for it to finish"))
		}
		return fail(err)
	}
	return file, nil
}

func updateInspectArchive(filename string) error {
	archive, err := zip.OpenReader(filename)
	if err != nil {
		return errors.New("Installer archive is invalid")
	}
	defer archive.Close()
	if len(archive.File) == 0 || len(archive.File) > 5000 {
		return errors.New("Installer archive exceeds its extraction limits")
	}
	names := make(map[string]bool)
	var expanded uint64
	fold := cases.Fold()
	for _, item := range archive.File {
		if item.UncompressedSize64 > updateMaxExpanded || expanded > updateMaxExpanded-item.UncompressedSize64 {
			return errors.New("Installer archive exceeds its extraction limits")
		}
		expanded += item.UncompressedSize64
		name := item.Name
		parts := strings.Split(strings.TrimSuffix(name, "/"), "/")
		unsafe := name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00") || len(parts) == 0 || (parts[0] != updateInstaller && parts[0] != "__MACOSX") || (!item.Mode().IsRegular() && !item.Mode().IsDir()) || item.Flags&1 != 0
		for _, part := range parts {
			if part == ".." || part == "." || part == "" {
				unsafe = true
			}
		}
		canonical := fold.String(norm.NFC.String(path.Clean(name)))
		if unsafe || names[canonical] {
			return errors.New("Installer archive contains unsafe paths or entries")
		}
		names[canonical] = true
	}
	return nil
}

func updateCommand(ctx context.Context, args []string) error {
	if ctx.Err() != nil {
		return updateNetworkError(ctx.Err(), ctx)
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(bounded, args[0], args[1:]...)
	if err := command.Run(); err != nil {
		if bounded.Err() != nil {
			return errors.New("Installer verification timed out. Try again")
		}
		return errors.New("Installer verification or opening failed. Download the installer from the release page")
	}
	return nil
}

type UpdateClient struct {
	Root, App, State string
	download         func(context.Context, string, int64, string) ([]byte, error)
	command          func(context.Context, []string) error
	pages            int
}

func NewUpdateClient(root, app, state string) *UpdateClient {
	if state == "" {
		state = filepath.Join(integrationHome(), ".local/state/agent-heavy")
	}
	httpClient := updateHTTPClient()
	return &UpdateClient{Root: root, App: app, State: state, download: func(ctx context.Context, url string, limit int64, dest string) ([]byte, error) {
		return updateDownload(ctx, httpClient, url, limit, dest)
	}, command: updateCommand, pages: updateMaxPages}
}

type updateSelection struct {
	manifest Manifest
	release  Object
}

func (client *UpdateClient) release(ctx context.Context) (*updateSelection, error) {
	var candidates []Object
	complete := false
	for page := 1; page <= client.pages; page++ {
		url := updateAPI
		if page > 1 {
			url += "&page=" + strconv.Itoa(page)
		}
		raw, err := client.download(ctx, url, 4*1024*1024, "")
		if err != nil {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, updateNetworkError(ctx.Err(), ctx)
		}
		var items []any
		if err := json.Unmarshal(raw, &items); err != nil || items == nil || len(items) > 100 {
			return nil, errors.New("Release service returned an invalid response")
		}
		for _, item := range items {
			release, ok := item.(map[string]any)
			if !ok || release["draft"] != false {
				continue
			}
			tag, ok := release["tag_name"].(string)
			if !ok || !updateTag.MatchString(tag) {
				continue
			}
			assets, ok := release["assets"].([]any)
			if !ok {
				continue
			}
			for _, value := range assets {
				if asset, ok := value.(map[string]any); ok && asset["name"] == "agent-job-queue.json" {
					candidates = append(candidates, Object(release))
					break
				}
			}
		}
		if len(items) < 100 {
			complete = true
			break
		}
	}
	if !complete {
		return nil, errors.New("Release search exceeded its limit. Open the project release page to check for updates")
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	newest := 0
	var release Object
	ambiguous := false
	for _, candidate := range candidates {
		match := updateTag.FindStringSubmatch(candidate["tag_name"].(string))
		build, _ := strconv.Atoi(match[1])
		if build > newest {
			newest = build
			release = candidate
			ambiguous = false
		} else if build == newest {
			ambiguous = true
		}
	}
	if ambiguous {
		return nil, errors.New("Published build identity is ambiguous")
	}
	asset, err := updateAsset(release, "agent-job-queue.json")
	if err != nil {
		return nil, err
	}
	raw, err := client.download(ctx, asset["browser_download_url"].(string), 16384, "")
	if err != nil {
		return nil, err
	}
	if err := updateVerifyDigest(raw, asset); err != nil {
		return nil, err
	}
	manifest, err := ValidateManifest(raw)
	if err != nil {
		return nil, fmt.Errorf("Published release metadata is invalid: %w", err)
	}
	if manifest.Tag != release["tag_name"] || *manifest.Commit != release["target_commitish"] {
		return nil, errors.New("Release identity does not match its manifest")
	}
	if _, err := updateAsset(release, manifest.Asset); err != nil {
		return nil, err
	}
	return &updateSelection{manifest, release}, nil
}

func (client *UpdateClient) check(ctx context.Context) (Object, *updateSelection, error) {
	installed, err := updateInstalled(client.Root, client.App)
	if err != nil {
		return nil, nil, err
	}
	selected, err := client.release(ctx)
	if err != nil {
		return nil, nil, err
	}
	var latest any
	comparison, message := "unavailable", "No published build is available"
	if selected != nil {
		manifest := selected.manifest
		latest = Object{"version": manifest.Version, "build": *manifest.Build, "commit": *manifest.Commit, "tag": manifest.Tag, "url": "https://github.com/" + ReleaseRepository + "/releases/tag/" + manifest.Tag}
		if installed["build"] == nil {
			comparison, message = "untracked", "Installed build is untracked. A published release is available"
		} else {
			build := installed["build"].(int)
			switch {
			case build == *manifest.Build:
				if installed["commit"] != *manifest.Commit || installed["version"] != manifest.Version {
					return nil, nil, errors.New("Published build number conflicts with the installed release identity")
				}
				comparison, message = "current", "You have the latest published build"
			case build > *manifest.Build:
				comparison, message = "newer", "Installed build is newer than the latest published release"
			default:
				comparison, message = "available", "An update is available"
			}
		}
	}
	return Object{"installed": installed, "latest": latest, "comparison": comparison, "canInstall": comparison == "available" || comparison == "untracked", "message": message}, selected, nil
}

func (client *UpdateClient) Check() (Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	status, _, err := client.check(ctx)
	return status, err
}

func (client *UpdateClient) verifyPackage(ctx context.Context, directory string, manifest Manifest) (string, error) {
	installer := filepath.Join(directory, updateInstaller)
	payload := filepath.Join(installer, "Contents/Resources/payload")
	app := filepath.Join(payload, "Agent Queue.app")
	for _, filename := range []string{filepath.Join(app, "Contents/Resources/build-info.json"), filepath.Join(payload, "source/build-info.json")} {
		raw, err := updateReadOwned(filename, 16384)
		if err != nil {
			return "", errors.New("Installer lacks matching build metadata. Use the release page for this older build")
		}
		info, err := ValidateBuildInfo(raw)
		if err != nil {
			return "", errors.New("Installer lacks matching build metadata. Use the release page for this older build")
		}
		if !info.Equal(manifest.BuildInfo) {
			return "", errors.New("Installer build identity does not match the published release")
		}
	}
	for _, bundle := range []struct{ path, id string }{{installer, "io.went.agent-queue.installer"}, {app, "io.went.agent-queue"}} {
		info, err := updatePlist(filepath.Join(bundle.path, "Contents/Info.plist"))
		if err != nil {
			return "", err
		}
		if info["CFBundleIdentifier"] != bundle.id || info["CFBundleShortVersionString"] != manifest.Version || info["CFBundleVersion"] != strconv.Itoa(*manifest.Build) {
			return "", errors.New("Installer bundle version does not match the published release")
		}
		if err := client.command(ctx, []string{"/usr/bin/codesign", "--verify", "--strict", "--deep", bundle.path}); err != nil {
			return "", err
		}
	}
	return installer, nil
}

func (client *UpdateClient) OpenInstaller(build int) (Object, error) {
	if build < 1 || build > MaxReleaseBuild {
		return nil, errors.New("Select a valid published build before downloading")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	lock, err := updateLock(client.State)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	status, selected, err := client.check(ctx)
	if err != nil {
		return nil, err
	}
	if status["canInstall"] != true || selected == nil || *selected.manifest.Build != build {
		return nil, errors.New("Update availability changed. Check for updates again before downloading")
	}
	manifest, release := selected.manifest, selected.release
	updates := filepath.Join(client.State, "updates")
	if err := updatePrivateDirectory(updates); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(updates)
	if err != nil {
		return nil, err
	}
	prepared := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "preparing-") && !strings.HasPrefix(name, "ready-") {
			continue
		}
		stage := filepath.Join(updates, name)
		if err := updatePrivateDirectory(stage); err != nil {
			return nil, err
		}
		if strings.HasPrefix(name, "preparing-") {
			if err := os.RemoveAll(stage); err != nil {
				return nil, err
			}
			continue
		}
		raw, err := updateReadOwned(filepath.Join(stage, "release.json"), 16384)
		if err != nil {
			return nil, err
		}
		prior, err := ValidateManifest(raw)
		if err != nil {
			return nil, err
		}
		installed := status["installed"].(Object)
		if installed["build"] != nil && *prior.Build <= installed["build"].(int) {
			if err := os.RemoveAll(stage); err != nil {
				return nil, err
			}
		} else {
			prepared++
		}
	}
	if prepared >= 3 {
		return nil, errors.New("Finish a previously opened installer before preparing another update")
	}
	temporary, err := os.MkdirTemp(updates, "preparing-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if temporary != "" {
			_ = os.RemoveAll(temporary)
		}
	}()
	archive := filepath.Join(temporary, manifest.Asset)
	asset, err := updateAsset(release, manifest.Asset)
	if err != nil {
		return nil, err
	}
	if _, err := client.download(ctx, asset["browser_download_url"].(string), updateMaxArchive, archive); err != nil {
		return nil, err
	}
	file, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	hasher := sha256.New()
	_, err = io.Copy(hasher, file)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if digest != manifest.SHA256 || (asset["digest"] != nil && asset["digest"] != "sha256:"+digest) {
		return nil, errors.New("Installer checksum does not match the published release")
	}
	if err := updateInspectArchive(archive); err != nil {
		return nil, err
	}
	expanded := filepath.Join(temporary, "expanded")
	if err := os.Mkdir(expanded, 0700); err != nil {
		return nil, err
	}
	if err := client.command(ctx, []string{"/usr/bin/ditto", "-x", "-k", archive, expanded}); err != nil {
		return nil, err
	}
	if _, err := client.verifyPackage(ctx, expanded, manifest); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(temporary, "release.json"), raw, 0600); err != nil {
		return nil, err
	}
	ready := filepath.Join(updates, "ready-"+strings.TrimPrefix(filepath.Base(temporary), "preparing-"))
	if err := os.Rename(temporary, ready); err != nil {
		return nil, err
	}
	temporary = ready
	installed, err := updateInstalled(client.Root, client.App)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(installed, status["installed"]) {
		return nil, errors.New("Installed version changed while downloading. Check for updates again")
	}
	if err := client.command(ctx, []string{"/usr/bin/open", "-n", filepath.Join(ready, "expanded", updateInstaller)}); err != nil {
		return nil, err
	}
	temporary = ""
	return Object{"installerOpened": true, "build": build, "message": "Installer opened. Setup starts automatically and waits for active jobs before updating background services"}, nil
}

func UpdatesMain(args []string) int {
	argumentError := func() int {
		fmt.Fprintln(os.Stderr, "agent-heavy: use version [--json], updates check [--json], or updates install --build NUMBER [--json]")
		return 2
	}
	if len(args) == 0 {
		return argumentError()
	}
	action := args[0]
	index := 1
	operation := ""
	if action == "updates" {
		if len(args) < 2 || (args[1] != "check" && args[1] != "install") {
			return argumentError()
		}
		operation = args[1]
		index = 2
	} else if action != "version" && action != "--version" {
		return argumentError()
	}
	jsonOutput, hasBuild := false, false
	build := 0
	for ; index < len(args); index++ {
		switch args[index] {
		case "--json":
			jsonOutput = true
		case "--build":
			if action != "updates" || hasBuild {
				return argumentError()
			}
			index++
			if index >= len(args) {
				return argumentError()
			}
			var err error
			build, err = strconv.Atoi(args[index])
			if err != nil {
				return argumentError()
			}
			hasBuild = true
		default:
			return argumentError()
		}
	}
	if action == "updates" && ((operation == "install") != hasBuild) {
		return argumentError()
	}
	var result Object
	var err error
	display := ""
	if action == "version" || action == "--version" {
		result, err = updateInstalled("", "")
		if err == nil {
			display = result["displayVersion"].(string)
		}
	} else {
		client := NewUpdateClient("", "", "")
		if operation == "check" {
			result, err = client.Check()
		} else {
			result, err = client.OpenInstaller(build)
		}
		if err == nil {
			display = result["message"].(string)
			if operation == "check" && result["latest"] != nil {
				latest := result["latest"].(Object)
				display += fmt.Sprintf("\nInstalled: %s\nLatest: %s (build %d)", result["installed"].(Object)["displayVersion"], latest["version"], latest["build"])
			}
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-heavy: %v\n", err)
		return 125
	}
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 125
		}
	} else {
		fmt.Println(display)
	}
	return 0
}
