package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"unicode"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"howett.net/plist"
)

// Size limits for the installer payload and owned reads.
const maxArchiveBytes = 128 * 1024 * 1024
const maxExpandedBytes = 512 * 1024 * 1024

func readOwnedFile(path string, limit int64) ([]byte, error) {
	return appservices.ReadFile(filepath.Dir(path), path, limit, false)
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func requireCompleteInstallation(root string) error {
	pending := filepath.Join(root, "install-in-progress.json")
	exists, err := pathExists(pending)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	data, err := readOwnedFile(pending, 16384)
	if err != nil {
		return err
	}
	if _, err := ValidateBuildInfo(data); err != nil {
		return err
	}
	return errors.New("Installation is incomplete. Open Agent Queue from Applications to finish setup before checking versions")
}

func readBundlePlist(path string) (map[string]any, error) {
	data, err := readOwnedFile(path, 16384)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if _, err := plist.Unmarshal(data, &value); err != nil {
		return nil, errors.New("Installed version metadata is invalid")
	}
	return value, nil
}

func isPrintableValue(value any) bool {
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

func installedVersionReport(root, app string) (Object, error) {
	if root == "" {
		root = filepath.Join(integrationHome(), ".local/share/agent-heavy")
	}
	if app == "" {
		var err error
		app, err = registeredAppPath()
		if err != nil {
			return nil, err
		}
	}
	if err := requireCompleteInstallation(root); err != nil {
		return nil, err
	}
	result, err := installedIdentity(root, app)
	if err != nil {
		return nil, err
	}
	if err := requireCompleteInstallation(root); err != nil {
		return nil, err
	}
	return result, nil
}

func registeredAppPath() (string, error) {
	home := integrationHome()
	if managed, err := appservices.Load(home); err != nil {
		return "", err
	} else if managed != nil {
		if err := requireCompleteInstallation(filepath.Join(home, ".local/share/agent-heavy")); err != nil {
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
	bundle, err := installPayloadParent(args[2], filepath.Join(args[2], ".version-probe"))
	if err != nil {
		return "", err
	}
	if err := bundle.Close(); err != nil {
		return "", err
	}
	return args[2], nil
}

func readAppFile(app, filename string) ([]byte, error) {
	if installValidateAppPath(integrationHome(), app) == nil {
		return installReadPayload(app, filename, 16384)
	}
	// Explicit development and archive paths retain their current-owner policy.
	return readOwnedFile(filename, 16384)
}

func readAppInfoPlist(app string) (map[string]any, error) {
	raw, err := readAppFile(app, filepath.Join(app, "Contents/Info.plist"))
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if _, err := plist.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("Installed version metadata is invalid")
	}
	return value, nil
}

func installedIdentity(root, app string) (Object, error) {
	marker, bundled := filepath.Join(root, "build-info.json"), filepath.Join(app, "Contents/Resources/build-info.json")
	exists, err := pathExists(marker)
	if err != nil {
		return nil, err
	}
	if exists {
		data, err := readOwnedFile(marker, 16384)
		if err != nil {
			return nil, err
		}
		info, err := ValidateBuildInfo(data)
		if err != nil {
			return nil, errors.New("Installed version metadata is invalid")
		}
		data, err = readAppFile(app, bundled)
		if err != nil {
			return nil, err
		}
		bundle, err := ValidateBuildInfo(data)
		if err != nil {
			return nil, errors.New("Installed version metadata is invalid")
		}
		if !info.Equal(bundle) {
			return nil, errors.New("App and controller versions differ. Complete installation before checking versions")
		}
		plist, err := readAppInfoPlist(app)
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
	exists, err = pathExists(bundled)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, errors.New("Installed controller version is missing. Complete installation before checking versions")
	}
	plistPath := filepath.Join(app, "Contents/Info.plist")
	exists, err = pathExists(plistPath)
	if err != nil {
		return nil, err
	}
	if !exists {
		return Object{"version": nil, "build": nil, "bundleBuild": nil, "commit": nil, "tracked": false, "displayVersion": "Not installed"}, nil
	}
	plist, err := readAppInfoPlist(app)
	if err != nil {
		return nil, err
	}
	version, build := plist["CFBundleShortVersionString"], plist["CFBundleVersion"]
	if plist["CFBundleIdentifier"] != "io.went.agent-queue" || !isPrintableValue(version) || !isPrintableValue(build) {
		return nil, errors.New("Installed app version is invalid")
	}
	return Object{"version": version, "build": nil, "bundleBuild": build, "commit": nil, "tracked": false, "displayVersion": fmt.Sprintf("%s (build %s, legacy installation)", version, build)}, nil
}

// VersionMain prints the installed version. The app delivers updates through Sparkle.
func VersionMain(args []string) int {
	jsonOutput := false
	for _, argument := range args[1:] {
		if argument != "--json" {
			fmt.Fprintln(os.Stderr, "agent-heavy: use version [--json]")
			return 2
		}
		jsonOutput = true
	}
	result, err := installedVersionReport("", "")
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
		fmt.Println(result["displayVersion"].(string))
	}
	return 0
}
