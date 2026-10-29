// Package appservices describes the installed, bundle-owned service boundary.
// It never registers services or changes files.
package appservices

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
	"howett.net/plist"
)

type Installation struct {
	SchemaVersion     int    `json:"schemaVersion"`
	App               string `json:"app"`
	ServiceManagement bool   `json:"serviceManagement"`
}

type Status struct {
	SchemaVersion int    `json:"schemaVersion"`
	Service       string `json:"service"`
	Status        string `json:"status"`
}

// ReadFile performs a bounded no-follow read. Bundle files may be root-owned;
// private metadata must belong to the current user, including its ancestors.
func ReadFile(boundary, filename string, limit int64, bundle bool) ([]byte, error) {
	return readFile(boundary, filename, limit, bundle, false)
}

func readFile(boundary, filename string, limit int64, bundle, executable bool) ([]byte, error) {
	boundary, filename = systemPath(boundary), systemPath(filename)
	if limit < 0 || limit > 512*1024*1024 {
		return nil, errors.New("invalid service metadata size limit")
	}
	if !filepath.IsAbs(boundary) || filepath.Clean(boundary) != boundary || !strings.HasPrefix(filename, boundary+"/") || filepath.Clean(filename) != filename {
		return nil, errors.New("invalid service metadata boundary")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := ""
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(filename), "/"), "/") {
		current += "/" + part
		child, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = child
		var st unix.Stat_t
		if err = unix.Fstat(fd, &st); err != nil {
			unix.Close(fd)
			return nil, err
		}
		if !serviceParentSafe(current, boundary, bundle, executable, &st) {
			unix.Close(fd)
			if executable {
				return nil, errors.New("executable location permits replacement by another account; use the explicit installer in a protected folder and install in your account's Applications directory")
			}
			return nil, errors.New("unsafe service metadata parent")
		}
	}
	defer unix.Close(fd)
	fileFD, err := unix.Openat(fd, filepath.Base(filename), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fileFD), filename)
	defer f.Close()
	var before, after unix.Stat_t
	if err := unix.Fstat(fileFD, &before); err != nil {
		return nil, err
	}
	if uint32(before.Mode)&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 || uint32(before.Mode)&0022 != 0 || before.Uid != uint32(os.Getuid()) && !(bundle && before.Uid == 0) {
		return nil, errors.New("unsafe service metadata file")
	}
	if before.Size > limit {
		return nil, errors.New("service metadata exceeds size limit")
	}
	if executable {
		if uint32(before.Mode)&0111 == 0 {
			return nil, errors.New("managed helper is not executable")
		}
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("service metadata exceeds size limit")
	}
	if err := unix.Fstat(fileFD, &after); err != nil {
		return nil, err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mode != after.Mode || before.Uid != after.Uid || before.Nlink != after.Nlink || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return nil, errors.New("service metadata changed during inspection")
	}
	return raw, nil
}

func serviceParentSafe(current, boundary string, bundle, executable bool, st *unix.Stat_t) bool {
	inside := current == boundary || strings.HasPrefix(current, boundary+"/")
	owner := st.Uid == uint32(os.Getuid()) || st.Uid == 0 && (!inside || bundle)
	// Shared Applications remains readable as package data. Executable paths
	// inherit that exception only from the stock admin-group directory.
	shared := !inside && st.Uid == 0 && (uint32(st.Mode)&unix.S_ISVTX != 0 ||
		current == "/Applications" && uint32(st.Mode)&0002 == 0 && (!executable || StockApplications(st)))
	return owner && (uint32(st.Mode)&0022 == 0 || shared)
}

// AdminGroup is the macOS admin group that owns the stock /Applications.
const AdminGroup = 80

// StockApplications reports whether st describes the stock /Applications
// directory: owned by root and writable by no one beyond root and the admin
// group. Administrators can already replace any code on this Mac, so Finder
// drag-and-drop installation there retains executable custody.
func StockApplications(st *unix.Stat_t) bool {
	return uint32(st.Mode)&unix.S_IFMT == unix.S_IFDIR && st.Uid == 0 && st.Gid == AdminGroup && uint32(st.Mode)&0002 == 0
}

// Resolve only macOS's fixed, root-owned aliases above a private boundary.
// Symlinks within homes, installation roots and bundles are always rejected.
func systemPath(path string) string {
	for _, alias := range []string{"/var", "/tmp"} {
		if path != alias && !strings.HasPrefix(path, alias+"/") {
			continue
		}
		var st unix.Stat_t
		if err := unix.Lstat(alias, &st); err != nil || uint32(st.Mode)&unix.S_IFMT != unix.S_IFLNK || st.Uid != 0 {
			continue
		}
		target, err := os.Readlink(alias)
		if err == nil && (target == "private"+alias || target == "/private"+alias) {
			return "/private" + path
		}
	}
	return path
}

// decode rejects duplicate keys as well as unknown fields and trailing values.
func decode(raw []byte, value any) error {
	if len(raw) > 16384 {
		return errors.New("service JSON exceeds size limit")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("invalid service JSON object")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return errors.New("duplicate service JSON field")
		}
		seen[name] = true
		var field json.RawMessage
		if err := d.Decode(&field); err != nil {
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing service JSON")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(value)
}

func validAppPath(home, app string) bool {
	return filepath.IsAbs(app) && filepath.Clean(app) == app && strings.HasSuffix(filepath.Base(app), ".app") && (filepath.Dir(app) == "/Applications" || filepath.Dir(app) == filepath.Join(home, "Applications"))
}

// Load returns nil only when installation.json is absent. Malformed or unsafe
// metadata never falls back to the legacy service layout.
func Load(home string) (*Installation, error) {
	root := filepath.Join(home, ".local/share/agent-heavy")
	raw, err := ReadFile(home, filepath.Join(root, "installation.json"), 16384, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var value Installation
	if err := decode(raw, &value); err != nil {
		return nil, err
	}
	if value.SchemaVersion != 1 || !value.ServiceManagement || !validAppPath(home, value.App) {
		return nil, errors.New("invalid managed installation metadata")
	}
	return &value, nil
}

// BundleForExecutable accepts only the exact canonical helper spellings.
func BundleForExecutable(executable string) (string, bool) {
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable || !slices.Contains([]string{"agent-heavy", "agent-remote"}, filepath.Base(executable)) {
		return "", false
	}
	app := filepath.Dir(filepath.Dir(filepath.Dir(executable)))
	return app, strings.HasSuffix(filepath.Base(app), ".app") && executable == filepath.Join(app, "Contents/MacOS", filepath.Base(executable))
}

// ValidateApp allows immutable reads of a Finder-installed root-owned bundle.
// Cryptographic verification is performed by the installer before publication
// and by the owning native executable before lifecycle operations.
func ValidateApp(home, app string) error {
	if !validAppPath(home, app) {
		return errors.New("managed app must be in Applications")
	}
	return ValidateBundle(app)
}

// ValidateBundle also supports an installer staging bundle outside Applications.
func ValidateBundle(app string) error {
	raw, err := ReadFile(app, filepath.Join(app, "Contents/Info.plist"), 16384, true)
	if err != nil {
		return err
	}
	var info map[string]any
	if _, err := plist.Unmarshal(raw, &info); err != nil {
		return err
	}
	if info["CFBundleIdentifier"] != "io.went.agent-queue" || info["AgentQueueServiceManagement"] != true {
		return errors.New("missing managed app packaging marker")
	}
	for _, name := range []string{"AgentQueue", "agent-heavy", "agent-remote"} {
		filename := filepath.Join(app, "Contents/MacOS", name)
		if _, err := readFile(app, filename, 128*1024*1024, true, true); err != nil {
			return err
		}
	}
	if _, err := ReadFile(app, filepath.Join(app, "Contents/Resources/payload/source/build-info.json"), 16384, true); err != nil {
		return err
	}
	for service, helper := range map[string]string{"heavy": "agent-heavy", "resources": "agent-heavy", "remote": "agent-remote"} {
		raw, err := ReadFile(app, filepath.Join(app, "Contents/Library/LaunchAgents/io.went.agent-"+service+".plist"), 16384, true)
		if err != nil {
			return err
		}
		var value map[string]any
		if _, err := plist.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value["Label"] != "io.went.agent-"+service || value["BundleProgram"] != "Contents/MacOS/"+helper {
			return errors.New("invalid managed service definition")
		}
		if _, exists := value["Program"]; exists {
			return errors.New("managed service overrides BundleProgram")
		}
		command := map[string]string{"heavy": "daemon", "resources": "scheduler", "remote": "serve"}[service]
		args, ok := value["ProgramArguments"].([]any)
		if !ok || len(args) != 2 || args[1] != command || args[0] != helper && args[0] != "Contents/MacOS/"+helper {
			return errors.New("managed service has unexpected helper arguments")
		}
	}
	// The support payload must never carry a second executable installation.
	if _, err := os.Lstat(filepath.Join(app, "Contents/Resources/payload/source/bin")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("managed app embeds private backend binaries")
	}
	return nil
}

func Arguments(app, action, service string) ([]string, error) {
	if !filepath.IsAbs(app) || filepath.Clean(app) != app || !strings.HasSuffix(filepath.Base(app), ".app") || !slices.Contains([]string{"status", "register", "unregister"}, action) || !slices.Contains([]string{"queue", "resources", "remote", "login"}, service) {
		return nil, errors.New("invalid service management command")
	}
	return []string{filepath.Join(app, "Contents/MacOS/AgentQueue"), "--service-management", action, service}, nil
}

func DecodeStatus(raw []byte, expectedService string) (Status, error) {
	var value Status
	if err := decode(raw, &value); err != nil {
		return Status{}, err
	}
	if !slices.Contains([]string{"queue", "resources", "remote", "login"}, expectedService) || value.SchemaVersion != 1 || value.Service != expectedService || !slices.Contains([]string{"notRegistered", "enabled", "requiresApproval", "notFound"}, value.Status) {
		return Status{}, fmt.Errorf("invalid native service status for %s", expectedService)
	}
	return value, nil
}

// LaunchServiceAbsent accepts only launchctl's exact user-domain absence
// witness. Permission errors, other labels and generic command failures cannot
// establish that it is safe to proceed without unregistering a service.
func LaunchServiceAbsent(output []byte, exitCode int, label string) bool {
	if !slices.Contains([]string{"io.went.agent-remote", "io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-queue"}, label) {
		return false
	}
	expected := fmt.Sprintf("Bad request.\nCould not find service \"%s\" in domain for user gui: %d", label, os.Getuid())
	return exitCode == 113 && strings.TrimSpace(string(output)) == expected
}
