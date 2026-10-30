package backend

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
	"howett.net/plist"
)

type installer struct {
	home, payload, root, state, config, launch, app, path string
	identity                                              BuildInfo
	pendingIdentity                                       *BuildInfo
	pendingBundle                                         *managedBundleIdentity
	setupStop                                             <-chan struct{}
	serviceSlot                                           string
	testing                                               bool
	appBundle                                             bool
	managed                                               bool
	queueSnapshot                                         func(context.Context) (Object, error)
	queueControl                                          func(context.Context, string, Object) (Object, error)
	disablePhoneAccess                                    bool
	prepareReplacement                                    bool
	unregisterServices                                    bool
	automaticSetup                                        bool
	command                                               func(context.Context, []string) ([]byte, error)
}

var errInstallLegacyPhoneAccess = errors.New("Phone access from the previous installation must be disabled before setup can continue. Credentials and configuration will be preserved")
var errInstallServiceNotLoaded = errors.New("Launch service is not loaded")
var errInstallPendingCleanup = errors.New("Queue migration must wait for queue work and cleanup to finish. Retained receipts or owned lease markers may still belong to running jobs. Existing files were preserved.")
var errInstallCustomCallback = errors.New("Custom cleanup callback cannot be retained by managed migration; it will be backed up before replacement")

type installDrainPendingError struct{ tasks Object }

func (err *installDrainPendingError) Error() string {
	return "Queue migration requires a drained queue. Finish or cancel all queued, stashed, running, and paused jobs, then open Agent Queue again"
}

type installApprovalError struct{ service string }

func (err *installApprovalError) Error() string {
	return fmt.Sprintf("The %s service is requiresApproval; allow Agent Queue in System Settings before setup can finish", err.service)
}

type installCleanupPendingError struct{ path string }

func (err *installCleanupPendingError) Error() string { return errInstallPendingCleanup.Error() }
func (err *installCleanupPendingError) Unwrap() error { return errInstallPendingCleanup }

const installDisablePhoneAccessRecovery = "AGENT_QUEUE_SETUP_RECOVERY:disable-phone-access"
const installLegacySnapshotLimit = 32 * 1024 * 1024

var installLegacyFiles = []string{"agent_heavy.py", "agent_integration.py", "resource_monitor.py", "job_profiles.py", "resource_scheduler.py", "learning_control.py", "app_updates.py", "release_metadata.py", "install-remote.sh", "scripts/migrate-installation.py", "remote/control.py", "remote/server.py", "remote/requirements.txt", "remote/requirements.lock"}
var installLegacyDirectories = []string{".venv", "__pycache__", "remote/__pycache__", "remote/static"}

func (install *installer) validateRetirement() error {
	for _, name := range append(append([]string{}, installLegacyFiles...), installLegacyDirectories...) {
		filename := filepath.Join(install.root, name)
		parent, err := integrationParent(filename, false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var info unix.Stat_t
		err = unix.Fstatat(int(parent.Fd()), filepath.Base(filename), &info, unix.AT_SYMLINK_NOFOLLOW)
		parent.Close()
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		directory := false
		for _, candidate := range installLegacyDirectories {
			if candidate == name {
				directory = true
				break
			}
		}
		kind := uint32(info.Mode) & unix.S_IFMT
		if info.Uid != uint32(os.Getuid()) || uint32(info.Mode)&0022 != 0 || (directory && kind != unix.S_IFDIR) || (!directory && (kind != unix.S_IFREG || info.Nlink != 1)) {
			return errors.New("Unsafe obsolete Python support ownership, permissions, or file type; resolve it before retrying installation")
		}
	}
	return nil
}

func installQuote(value string) string {
	if integrationShellSafe.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func installDirectory(directory string) error {
	parent, err := integrationParent(filepath.Join(directory, ".installer-probe"), true)
	if err != nil {
		return err
	}
	defer parent.Close()
	return unix.Fchmod(int(parent.Fd()), 0700)
}

func installAtomic(filename string, data []byte, mode os.FileMode) error {
	parent, err := integrationParent(filename, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(filename)
	fd := int(parent.Fd())
	var old unix.Stat_t
	if err := unix.Fstatat(fd, name, &old, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if uint32(old.Mode)&unix.S_IFMT != unix.S_IFREG || old.Uid != uint32(os.Getuid()) || old.Nlink != 1 || uint32(old.Mode)&0022 != 0 {
			return errors.New("Unsafe installed file ownership or permissions")
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	temporary := ".install-" + token() + ".tmp"
	defer unix.Unlinkat(fd, temporary, 0)
	if err := integrationPrivateWrite(parent, temporary, data); err != nil {
		return err
	}
	if err := unix.Fchmodat(fd, temporary, uint32(mode.Perm()), unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Renameat(fd, temporary, fd, name); err != nil {
		return err
	}
	return parent.Sync()
}

func (install *installer) executable(name string) (string, error) {
	for _, directory := range filepath.SplitList(install.path) {
		filename := filepath.Join(directory, name)
		info, err := os.Stat(filename)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return filename, nil
		}
	}
	return "", fmt.Errorf("Required tool %s is unavailable", name)
}

func installCommandContext(ctx context.Context, args []string) (context.Context, context.CancelFunc) {
	if len(args) > 1 && (args[1] == "--service-management" || args[1] == "--prepare-app-replacement") {
		// The native bridge has its own 30-second completion deadline. Allow
		// startup and delivery of its failure result before the outer bound.
		timeout := appservices.NativeCommandTimeout
		if len(args) == 4 && args[1] == "--service-management" && args[2] == "status" {
			timeout = appservices.NativeStatusTimeout
		}
		return context.WithTimeout(ctx, timeout)
	}
	if len(args) == 2 && filepath.Base(args[0]) == "agent-remote" && slices.Contains([]string{"migrate", "disable"}, args[1]) {
		return context.WithTimeout(ctx, appservices.ControllerCommandTimeout)
	}
	return context.WithTimeout(ctx, 30*time.Second)
}

func (install *installer) commandEnvironment(args []string) []string {
	environment := envMap()
	environment["PATH"] = install.path
	environment["HOMEBREW_NO_AUTO_UPDATE"] = "1"
	managedRemote := len(args) == 2 && (args[1] == "disable" || args[1] == "migrate") && (args[0] == filepath.Join(install.app, "Contents/MacOS/agent-remote") || args[0] == filepath.Join(install.payload, "Agent Queue.app/Contents/MacOS/agent-remote"))
	if managedRemote || slices.Equal(args, []string{filepath.Join(install.payload, "source/bin/agent-remote"), "disable"}) || len(args) > 0 && args[0] == "pueue" {
		// Recovery targets this installation, including when launched from a
		// developer shell with private remote-controller fixture overrides.
		for key := range environment {
			if strings.HasPrefix(key, "AGENT_REMOTE_") || strings.HasPrefix(key, "AGENT_HEAVY_") || strings.HasPrefix(key, "PUEUE_") {
				delete(environment, key)
			}
		}
		environment["HOME"] = install.home
	}
	replacement := len(args) == 3 && args[1] == "--prepare-app-replacement" && args[2] == install.app && (args[0] == filepath.Join(install.app, "Contents/MacOS/AgentQueue") || args[0] == filepath.Join(install.payload, "Agent Queue.app/Contents/MacOS/AgentQueue"))
	if replacement || slices.Equal(args, []string{filepath.Join(install.root, "bin/agent-heavy"), "status", "--json"}) || slices.Equal(args, []string{filepath.Join(install.app, "Contents/MacOS/agent-heavy"), "status", "--json"}) || len(args) > 1 && args[0] == filepath.Join(install.app, "Contents/MacOS/AgentQueue") && args[1] == "--service-management" {
		// Installation always targets this user's installed state. An ambient
		// developer queue override must never establish readiness for that target.
		for key := range environment {
			if strings.HasPrefix(key, "AGENT_HEAVY_") || strings.HasPrefix(key, "PUEUE_") {
				delete(environment, key)
			}
		}
		environment["HOME"] = install.home
		environment["AGENT_HEAVY_STATE"] = install.state
		environment["AGENT_HEAVY_CONFIG"] = filepath.Join(install.config, "pueue.yml")
		if install.serviceSlot != "" {
			environment["AGENT_HEAVY_LEGACY_SLOT"] = install.serviceSlot
		}
	}
	return envList(environment)
}

func (install *installer) commandOutputLimit(args []string) int {
	if slices.Equal(args, []string{"pueue", "--config", filepath.Join(install.config, "pueue.yml"), "--color", "never", "status", "--json"}) {
		// Loaded and saved legacy state have the same bounded history budget.
		return installLegacySnapshotLimit
	}
	return 4 * 1024 * 1024
}

func (install *installer) run(ctx context.Context, args ...string) ([]byte, error) {
	if install.command != nil {
		return install.command(ctx, args)
	}
	environmentArgs := args
	if install.testing && len(args) > 1 && (args[1] == "--service-management" || args[1] == "--prepare-app-replacement") {
		privateArgs, err := install.privateManagedCommand(args)
		if err != nil {
			return nil, err
		}
		args = privateArgs
	}
	commandPath := args[0]
	if !filepath.IsAbs(commandPath) {
		var err error
		commandPath, err = install.executable(commandPath)
		if err != nil {
			return nil, err
		}
	}
	bounded, cancel := installCommandContext(ctx, args)
	defer cancel()
	command := exec.CommandContext(bounded, commandPath, args[1:]...)
	command.Env = install.commandEnvironment(environmentArgs)
	output := &boundedOutput{limit: install.commandOutputLimit(environmentArgs)}
	command.Stdout = output
	command.Stderr = output
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		if bounded.Err() != nil {
			return nil, fmt.Errorf("Installer command %s timed out", filepath.Base(args[0]))
		}
		var exited *exec.ExitError
		if len(args) == 3 && args[0] == "launchctl" && args[1] == "print" && strings.HasPrefix(args[2], install.domain()+"/") && errors.As(err, &exited) &&
			installServiceAbsent(output.Bytes(), exited.ExitCode(), strings.TrimPrefix(args[2], install.domain()+"/")) {
			return nil, errInstallServiceNotLoaded
		}
		return nil, fmt.Errorf("Installer command %s failed: %w", filepath.Base(args[0]), err)
	}
	return output.Bytes(), nil
}

func installRemoteServiceAbsent(output []byte, exitCode int) bool {
	return installServiceAbsent(output, exitCode, "io.went.agent-remote")
}

func installServiceAbsent(output []byte, exitCode int, label string) bool {
	return appservices.LaunchServiceAbsent(output, exitCode, label)
}

func (install *installer) domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }
func (install *installer) loaded(ctx context.Context, label string) bool {
	_, err := install.run(ctx, "launchctl", "print", install.domain()+"/"+label)
	return err == nil
}

func (install *installer) validateServices() error {
	configuredSlot, err := install.configuredSlot()
	if err != nil {
		return err
	}
	serviceSlot := ""
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources"} {
		filename := filepath.Join(install.launch, label+".plist")
		exists, err := updateExists(filename)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		value, err := updatePlist(filename)
		if err != nil {
			return err
		}
		if value["Label"] != label {
			return errors.New("Existing launch agent has an unexpected service identity")
		}
		args, err := stringsFrom(value["ProgramArguments"])
		if err != nil {
			return err
		}
		if raw, present := value["Program"]; present {
			program, valid := raw.(string)
			if !valid || len(args) == 0 || program != args[0] {
				return errors.New("Existing launch agent Program differs from its expected executable; preserve its behavior before opening Agent Queue again")
			}
		}
		valid := false
		if label == "io.went.agent-heavy" {
			valid = (len(args) == 2 && args[0] == filepath.Join(install.root, "bin/agent-heavy") && args[1] == "daemon") || (len(args) == 3 && filepath.IsAbs(args[0]) && filepath.Base(args[0]) == "pueued" && (args[1] == "--config" || args[1] == "-c") && args[2] == filepath.Join(install.config, "pueue.yml"))
		} else {
			valid = (len(args) == 2 && args[0] == filepath.Join(install.root, "bin/agent-heavy") && args[1] == "scheduler") || (len(args) == 3 && args[0] == filepath.Join(install.root, ".venv/bin/python") && args[1] == filepath.Join(install.root, "agent_heavy.py") && args[2] == "scheduler")
		}
		if !valid {
			return errors.New("Existing launch agent is customized; preserve its behavior before opening Agent Queue again")
		}
		environment := Object{}
		if raw, present := value["EnvironmentVariables"]; present {
			environment = obj(raw)
			if environment == nil {
				return errors.New("Existing launch agent environment is invalid")
			}
		}
		for key, expected := range map[string]string{"HOME": install.home, "AGENT_HEAVY_STATE": install.state, "AGENT_HEAVY_CONFIG": filepath.Join(install.config, "pueue.yml")} {
			if raw, present := environment[key]; present {
				actual, valid := raw.(string)
				if !valid || actual != expected {
					return fmt.Errorf("Existing launch agent %s differs from the installation target; preserve its configuration before opening Agent Queue again", key)
				}
			}
		}
		slot := configuredSlot
		if raw, present := environment["AGENT_HEAVY_LEGACY_SLOT"]; present {
			actual, valid := raw.(string)
			if !valid || !integrationValidPath(actual) {
				return errors.New("Existing launch agent AGENT_HEAVY_LEGACY_SLOT must name a valid absolute path")
			}
			slot = actual
		}
		if serviceSlot != "" && serviceSlot != slot {
			return errors.New("Existing queue and resource controller use different legacy slots; reconcile their configuration before opening Agent Queue again")
		}
		serviceSlot = slot
	}
	if serviceSlot == "" {
		serviceSlot = configuredSlot
	}
	install.serviceSlot = serviceSlot
	return nil
}

func (install *installer) configuredSlot() (string, error) {
	if slot, err := install.legacySlot(); err != nil || slot != "" {
		return slot, err
	}
	settings, err := ReadPrivate(filepath.Join(install.state, "legacy-slot.json"))
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Join(install.state, "execution.slot"), nil
	}
	if err != nil {
		return "", err
	}
	return text(settings["slot"]), nil
}

func installReadInfo(filename string) (BuildInfo, error) {
	raw, err := updateReadOwned(filename, 16384)
	if err != nil {
		return BuildInfo{}, err
	}
	return ValidateBuildInfo(raw)
}

// A downloaded payload replaces the recorded installation, including shared
// Applications and renamed bundles. An explicit app-bundle target stays explicit
// so an ownership-checked relocation never redirects the caller to the old app.
func (install *installer) resolvePayloadApp() error {
	if install.appBundle {
		return nil
	}
	metadata, err := appservices.Load(install.home)
	if err != nil {
		return err
	}
	if metadata != nil {
		install.app = metadata.App
	}
	return nil
}

func (install *installer) validateAppPath() error {
	if err := installValidateAppPath(install.home, install.app); err != nil {
		return err
	}
	if install.payload != filepath.Join(install.app, "Contents/Resources/payload") {
		return errors.New("App bundle payload must be embedded in Contents/Resources/payload")
	}
	return nil
}

func installValidateAppPath(home, app string) error {
	if !integrationValidPath(app) || filepath.Clean(app) != app || !strings.HasSuffix(filepath.Base(app), ".app") {
		return errors.New("App bundle must be a direct application in /Applications or your home Applications directory")
	}
	parent := filepath.Dir(app)
	if parent != "/Applications" && parent != filepath.Join(home, "Applications") {
		return errors.New("Move Agent Queue to /Applications or your home Applications directory before setup")
	}
	return nil
}

// Open every parent without following symlinks. Root ownership is permitted only
// for immutable bundle resources, never for installed private state or locks.
// /Applications is an Apple-managed shared parent and must not be chmodded.
func installPayloadParent(root, filename string, bundled bool) (*os.File, error) {
	if !integrationValidPath(root) || filepath.Clean(root) != root || !integrationValidPath(filename) || filepath.Clean(filename) != filename || !strings.HasPrefix(filename, root+string(filepath.Separator)) {
		return nil, errors.New("Installer read escapes its canonical payload boundary")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := ""
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(filename), "/"), "/") {
		current += "/" + part
		child, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = child
		var info unix.Stat_t
		if err := unix.Fstat(fd, &info); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		inside := current == root || strings.HasPrefix(current, root+"/")
		owner := info.Uid == uint32(os.Getuid()) || (info.Uid == 0 && (!inside || bundled))
		sharedTemporary := !inside && info.Uid == 0 && uint32(info.Mode)&unix.S_ISVTX != 0
		sharedApplications := !inside && current == "/Applications" && info.Uid == 0 && uint32(info.Mode)&0002 == 0
		if !owner || (uint32(info.Mode)&0022 != 0 && !sharedTemporary && !sharedApplications) {
			_ = unix.Close(fd)
			return nil, errors.New("Installer payload has unsafe parent ownership or permissions")
		}
	}
	return os.NewFile(uintptr(fd), filepath.Dir(filename)), nil
}

func installReadPayload(root, filename string, limit int64, bundled bool) ([]byte, error) {
	parent, err := installPayloadParent(root, filename, bundled)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(filename), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filename)
	defer file.Close()
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return nil, err
	}
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFREG || (info.Uid != uint32(os.Getuid()) && !(bundled && info.Uid == 0)) || uint32(info.Mode)&0022 != 0 || info.Nlink != 1 {
		return nil, errors.New("Installer payload has unsafe ownership, permissions, or file type")
	}
	before := integrationStatIdentity(&info)
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("Installer payload exceeds its size limit")
	}
	if err := unix.Fstat(fd, &info); err != nil {
		return nil, err
	}
	if before != integrationStatIdentity(&info) {
		return nil, errors.New("Installer payload changed during inspection")
	}
	return data, nil
}

func (install *installer) readPayload(filename string, limit int64) ([]byte, error) {
	root := install.payload
	if install.appBundle {
		root = install.app
	}
	return installReadPayload(root, filename, limit, install.appBundle)
}

func installValidatePayloadTree(root string, bundled bool) error {
	count := 0
	var total int64
	return filepath.WalkDir(root, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 10000 {
			return errors.New("Application payload exceeds its file limit")
		}
		if entry.IsDir() {
			parent, err := installPayloadParent(root, filepath.Join(filename, ".installer-probe"), bundled)
			if err != nil {
				return err
			}
			return parent.Close()
		}
		if !entry.Type().IsRegular() {
			return errors.New("Application payload contains a symlink or special file")
		}
		parent, err := installPayloadParent(root, filename, bundled)
		if err != nil {
			return err
		}
		var info unix.Stat_t
		err = unix.Fstatat(int(parent.Fd()), filepath.Base(filename), &info, unix.AT_SYMLINK_NOFOLLOW)
		parent.Close()
		if err != nil {
			return err
		}
		if uint32(info.Mode)&unix.S_IFMT != unix.S_IFREG || (info.Uid != uint32(os.Getuid()) && !(bundled && info.Uid == 0)) || uint32(info.Mode)&0022 != 0 || info.Nlink != 1 {
			return errors.New("Installer payload has unsafe ownership, permissions, or file type")
		}
		total += info.Size
		if info.Size > updateMaxArchive || total > updateMaxExpanded {
			return errors.New("Application payload exceeds its size limit")
		}
		return nil
	})
}

func (install *installer) validatePayload() error {
	if !install.appBundle {
		// Classic installer sources may have a system alias above the payload
		// (for example macOS temporary storage). Establish its canonical boundary
		// first; every path inside that boundary still rejects symlinks.
		if !integrationValidPath(install.payload) {
			return errors.New("Installer payload must be an absolute path without traversal")
		}
		payload, err := filepath.EvalSymlinks(install.payload)
		if err != nil {
			return err
		}
		install.payload = payload
	}
	app := filepath.Join(install.payload, "Agent Queue.app")
	boundary := install.payload
	if install.appBundle {
		if err := install.validateAppPath(); err != nil {
			return err
		}
		app = install.app
		boundary = app
		if exists, err := updateExists(filepath.Join(install.payload, "Agent Queue.app")); err != nil {
			return err
		} else if exists {
			return errors.New("App bundle payload must not contain a recursive Agent Queue.app")
		}
	}
	if err := installValidatePayloadTree(boundary, install.appBundle); err != nil {
		return err
	}
	metadata, err := install.readPayload(filepath.Join(app, "Contents/Info.plist"), 16384)
	if err != nil {
		return err
	}
	var packaging map[string]any
	if _, err := plist.Unmarshal(metadata, &packaging); err != nil {
		return err
	}
	install.managed = false
	if marker, present := packaging["AgentQueueServiceManagement"]; present {
		if marker != true {
			return errors.New("Invalid service management packaging marker")
		}
		install.managed = true
		if err := appservices.ValidateBundle(app); err != nil {
			return err
		}
	}
	executables := []string{filepath.Join(app, "Contents/MacOS/AgentQueue")}
	if !install.managed || !install.appBundle {
		executables = append(executables, filepath.Join(install.payload, "source/bin/agent-heavy"), filepath.Join(install.payload, "source/bin/agent-remote"))
	}
	for _, filename := range executables {
		if _, err := install.readPayload(filename, updateMaxArchive); err != nil {
			return err
		}
		info, err := os.Lstat(filename)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("Incomplete installer payload")
		}
		if info.Mode().Perm()&0111 == 0 {
			return errors.New("Installer backend binaries must be executable")
		}
	}
	raw, err := install.readPayload(filepath.Join(install.payload, "source/build-info.json"), 16384)
	if err != nil {
		return err
	}
	identity, err := ValidateBuildInfo(raw)
	if err != nil {
		return err
	}
	if install.pendingIdentity != nil && !identity.Equal(*install.pendingIdentity) {
		return errors.New("The retained app changed from the exact pending build; recovery stopped")
	}
	if install.pendingIdentity != nil {
		pending, err := installReadInfo(filepath.Join(install.root, "install-in-progress.json"))
		if err != nil || !pending.Equal(*install.pendingIdentity) {
			return errors.New("The pending installation changed; recovery stopped")
		}
	}
	if install.pendingBundle != nil {
		if err := managedRequireBundle(install.app, *install.pendingBundle); err != nil {
			return err
		}
	}
	install.identity = identity
	raw, err = install.readPayload(filepath.Join(app, "Contents/Resources/build-info.json"), 16384)
	if err != nil {
		return err
	}
	bundled, err := ValidateBuildInfo(raw)
	if err != nil {
		return err
	}
	if !identity.Equal(bundled) {
		return errors.New("Installer app and support build identities differ")
	}
	if install.managed {
		raw, err := install.readPayload(filepath.Join(app, "Contents/Resources/payload/source/build-info.json"), 16384)
		if err != nil {
			return err
		}
		embedded, err := ValidateBuildInfo(raw)
		if err != nil {
			return err
		}
		if !identity.Equal(embedded) {
			return errors.New("Managed app and embedded support identities differ")
		}
	}
	raw, err = install.readPayload(filepath.Join(app, "Contents/Info.plist"), 16384)
	if err != nil {
		return err
	}
	var info map[string]any
	if _, err := plist.Unmarshal(raw, &info); err != nil {
		return errors.New("Installer app metadata is invalid")
	}
	build := "0"
	if identity.Build != nil {
		build = strconv.Itoa(*identity.Build)
	}
	if info["CFBundleIdentifier"] != "io.went.agent-queue" || info["CFBundleShortVersionString"] != identity.Version || info["CFBundleVersion"] != build {
		return errors.New("Installer app version differs from its build identity")
	}
	markers := []string{filepath.Join(install.root, "build-info.json"), filepath.Join(install.root, "install-in-progress.json")}
	if !install.appBundle {
		markers = append(markers, filepath.Join(install.app, "Contents/Resources/build-info.json"))
	}
	for _, marker := range markers {
		exists, err := updateExists(marker)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		var installed BuildInfo
		if strings.HasPrefix(marker, install.app+"/") {
			data, readErr := installReadPayload(install.app, marker, 16384, true)
			if readErr != nil {
				return readErr
			}
			installed, err = ValidateBuildInfo(data)
		} else {
			installed, err = installReadInfo(marker)
		}
		if err != nil {
			return err
		}
		if install.unregisterServices {
			// Removal verifies identities but never activates code. An installed
			// old app can still be removed after preparation of a newer candidate.
			continue
		}
		if install.managed && installed.Build != nil && identity.Build == nil {
			return errors.New("Refusing to replace a tracked completed or pending installation with an untracked build")
		}
		if identity.Build != nil && installed.Build != nil {
			if *identity.Build < *installed.Build {
				return errors.New("Refusing to install an older tracked build over a newer installation")
			}
			if *identity.Build == *installed.Build && !identity.Equal(installed) {
				return errors.New("The same tracked build number has a different commit or version")
			}
		}
		if install.managed && marker == filepath.Join(install.root, "install-in-progress.json") && !identity.Equal(installed) {
			superseded, err := install.managedPendingSuperseded(identity, installed)
			if err != nil {
				return err
			}
			if !superseded {
				return &installPendingError{identity: installed}
			}
		}
	}
	return nil
}

func installLock(state, name string) (*os.File, error) {
	fd, err := unix.Open(filepath.Join(state, name), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		file.Close()
		return nil, err
	}
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Getuid()) || info.Nlink != 1 || uint32(info.Mode)&0077 != 0 {
		file.Close()
		return nil, errors.New("Unsafe installer coordination lock")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("Another installer or queue change is running; finish it before trying again: %w", err)
	}
	return file, nil
}

func installYAMLValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return nil
		}
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func installConfig(filename string) (*yaml.Node, error) {
	raw, err := updateReadOwned(filename, 4*1024*1024)
	if err != nil {
		return nil, err
	}
	return installDecodeConfig(raw)
}

func installDecodeConfig(raw []byte) (*yaml.Node, error) {
	var node yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&node); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("Queue configuration must contain one YAML document")
	}
	var parsed map[string]any
	if err := node.Decode(&parsed); err != nil || parsed == nil {
		return nil, errors.New("Invalid queue configuration")
	}
	return &node, nil
}

func installConfigCallback(node *yaml.Node) (string, error) {
	daemon := installYAMLValue(node, "daemon")
	if daemon == nil {
		return "", errors.New("Queue configuration lacks daemon settings")
	}
	callback := installYAMLValue(daemon, "callback")
	if callback == nil || callback.Kind != yaml.ScalarNode || callback.Tag != "!!str" {
		return "", errors.New("Queue configuration lacks an explicit callback")
	}
	return callback.Value, nil
}

// Match literal spellings only, without interpreting shell syntax. In particular,
// double quotes are equivalent only when the path contains no shell expansions
// or escapes. installQuote also handles literal apostrophes in installed paths.
func installCallbackPathSpellings(path string) []string {
	spellings := []string{installQuote(path), "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"}
	if !strings.ContainsAny(path, "\\\"$`\r\n") {
		spellings = append(spellings, "\""+path+"\"")
	}
	return spellings
}

func (install *installer) standardCleanupCallback(callback string) bool {
	const suffix = " cleanup {{ id }}"
	if !strings.HasSuffix(callback, suffix) {
		return false
	}
	invocation := strings.TrimSuffix(callback, suffix)
	if slices.Contains(installCallbackPathSpellings(filepath.Join(install.root, "bin/agent-heavy")), invocation) {
		return true
	}
	if install.managed && slices.Contains(installCallbackPathSpellings(filepath.Join(install.home, ".local/bin/agent-heavy")), invocation) {
		return true
	}
	// The legacy installer used a venv and Homebrew Python 3.12. Retain only
	// these and standard Python 3 locations on Apple Silicon and Intel Macs;
	// arbitrary basenames, wrappers, Cellar paths and flags are not equivalent.
	interpreters := []string{
		filepath.Join(install.root, ".venv/bin/python"),
		filepath.Join(install.root, ".venv/bin/python3"),
		filepath.Join(install.root, ".venv/bin/python3.12"),
		"python3", "python3.12", "/usr/bin/python3",
		"/opt/homebrew/bin/python3", "/opt/homebrew/bin/python3.12",
		"/usr/local/bin/python3", "/usr/local/bin/python3.12",
		"/opt/homebrew/opt/python@3.12/bin/python3.12",
		"/usr/local/opt/python@3.12/bin/python3.12",
	}
	for _, interpreter := range interpreters {
		for _, executable := range installCallbackPathSpellings(interpreter) {
			for _, script := range installCallbackPathSpellings(filepath.Join(install.root, "agent_heavy.py")) {
				if invocation == executable+" "+script {
					return true
				}
			}
		}
	}
	return false
}

func installDecodeLegacySnapshot(raw []byte) (Object, error) {
	value, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	tasks := obj(value["tasks"])
	if tasks == nil {
		return nil, errors.New("Cannot safely inspect existing Pueue tasks")
	}
	if _, _, err := legacyQueueSettings(value); err != nil {
		return nil, fmt.Errorf("Cannot safely inspect existing legacy queue settings: %w", err)
	}
	return value, nil
}

func installRequireDrained(tasks Object) error {
	unfinished := false
	for _, raw := range tasks {
		task := obj(raw)
		if task == nil {
			return errors.New("Cannot safely inspect an existing Pueue task")
		}
		status, _, err := TaskStatus(task)
		if err != nil {
			return err
		}
		if status != "Done" {
			unfinished = true
		}
	}
	if unfinished {
		return &installDrainPendingError{tasks: tasks}
	}
	return nil
}

func (install *installer) legacySnapshot(ctx context.Context, node *yaml.Node, loaded bool) (Object, error) {
	config := filepath.Join(install.config, "pueue.yml")
	if loaded {
		raw, err := install.run(ctx, "pueue", "--config", config, "--color", "never", "status", "--json")
		if err != nil {
			return nil, err
		}
		return installDecodeLegacySnapshot(raw)
	}
	directory := filepath.Join(install.state, "pueue")
	if shared := installYAMLValue(node, "shared"); shared != nil {
		if configured := installYAMLValue(shared, "pueue_directory"); configured != nil {
			if configured.Kind != yaml.ScalarNode || !integrationValidPath(configured.Value) {
				return nil, errors.New("Cannot safely inspect the existing Pueue directory")
			}
			directory = configured.Value
		}
	}
	filename := filepath.Join(directory, "state.json")
	exists, err := updateExists(filename)
	if err != nil {
		return nil, err
	}
	if !exists {
		if compressed, err := updateExists(filepath.Join(directory, "state.json.gz")); err != nil {
			return nil, err
		} else if compressed {
			filename = filepath.Join(directory, "state.json.gz")
		} else {
			return Object{"tasks": Object{}, "groups": Object{"default": Object{"parallel_tasks": 1, "status": "Running"}}}, nil
		}
	}
	raw, err := updateReadOwned(filename, installLegacySnapshotLimit)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(filename, ".gz") {
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		raw, err = io.ReadAll(io.LimitReader(reader, installLegacySnapshotLimit+1))
		if err != nil || len(raw) > installLegacySnapshotLimit {
			return nil, errors.New("Cannot safely inspect compressed legacy queue state")
		}
	}
	snapshot, err := installDecodeLegacySnapshot(raw)
	if err != nil {
		return nil, err
	}
	// Retain access to legacy output without moving or rewriting private logs.
	for id, raw := range obj(snapshot["tasks"]) {
		task := obj(raw)
		if task != nil {
			task["logPath"] = filepath.Join(directory, "task_logs", id+".log")
		}
	}
	return snapshot, nil
}

func (install *installer) tasks(ctx context.Context, node *yaml.Node, loaded bool) (Object, error) {
	snapshot, err := install.legacySnapshot(ctx, node, loaded)
	if err != nil {
		return nil, err
	}
	return obj(snapshot["tasks"]), nil
}

func (install *installer) queueMigration(ctx context.Context, node *yaml.Node) (bool, error) {
	filename := filepath.Join(install.launch, "io.went.agent-heavy.plist")
	exists, err := updateExists(filename)
	if err != nil {
		return false, err
	}
	if exists {
		definition, err := updatePlist(filename)
		if err != nil {
			return false, err
		}
		args, err := stringsFrom(definition["ProgramArguments"])
		if err != nil {
			return false, err
		}
		if len(args) == 2 && args[0] == filepath.Join(install.root, "bin/agent-heavy") && args[1] == "daemon" {
			return false, nil
		}
	} else if install.loaded(ctx, "io.went.agent-heavy") {
		return false, errors.New("Cannot establish ownership of the running queue without its launch agent definition")
	} else if node == nil {
		for _, name := range []string{"state.json", "state.json.gz"} {
			if exists, err := updateExists(filepath.Join(install.state, "pueue", name)); err != nil {
				return false, err
			} else if exists {
				return false, errors.New("Legacy queue state exists without its dedicated configuration; restore the configuration before retrying installation")
			}
		}
		return false, nil
	} else if database, err := updateExists(filepath.Join(install.state, "queue.sqlite3")); err != nil {
		return false, err
	} else if database {
		return false, nil
	}
	if node == nil {
		return false, errors.New("Cannot inspect the legacy queue without its dedicated configuration")
	}
	callback, err := installConfigCallback(node)
	if err != nil {
		return false, err
	}
	if !install.standardCleanupCallback(callback) {
		return false, errors.New("Existing queue callback is customized; preserve its behavior before migrating the queue")
	}
	tasks, err := install.tasks(ctx, node, install.loaded(ctx, "io.went.agent-heavy"))
	if err != nil {
		return false, err
	}
	if err := installRequireDrained(tasks); err != nil {
		return false, err
	}
	return true, install.requireLegacyCleanup()
}

func (install *installer) requireLegacyCleanup() error {
	receipts, err := integrationParent(filepath.Join(install.state, "receipts", ".installer-probe"), false)
	if err == nil {
		entries, readErr := receipts.ReadDir(-1)
		receipts.Close()
		if readErr != nil {
			return readErr
		}
		if len(entries) != 0 {
			return &installCleanupPendingError{path: filepath.Join(install.state, "receipts")}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	slot := install.serviceSlot
	if slot == "" {
		slot, err = install.configuredSlot()
		if err != nil {
			return err
		}
	}
	// The shared slot may be occupied by an unrelated validator. Only Agent
	// Queue's own marker names establish unfinished queue cleanup here.
	for _, name := range []string{"agent-heavy.json", "agent-heavy-lease.json"} {
		if _, err := os.Lstat(filepath.Join(slot, name)); err == nil {
			return &installCleanupPendingError{path: filepath.Join(slot, name)}
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOTDIR) {
			return err
		}
	}
	jobs, err := integrationParent(filepath.Join(slot, "agent-heavy-jobs", ".installer-probe"), false)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
		return nil
	}
	if err != nil {
		return err
	}
	defer jobs.Close()
	entries, err := jobs.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return &installCleanupPendingError{path: filepath.Join(slot, "agent-heavy-jobs")}
	}
	return nil
}

func (install *installer) embeddedDefinition() bool {
	definition, err := updatePlist(filepath.Join(install.launch, "io.went.agent-heavy.plist"))
	if err != nil {
		return false
	}
	args, err := stringsFrom(definition["ProgramArguments"])
	return err == nil && slices.Equal(args, []string{filepath.Join(install.root, "bin/agent-heavy"), "daemon"})
}

var installSlotAssignment = regexp.MustCompile(`(?m)^\s*self\.slot\s*=\s*(.+?)\s*$`)
var installSlotLookup = regexp.MustCompile(`^Path\(\s*os\.environ\.get\(\s*(?:OVERRIDES\[2\]|["']AGENT_HEAVY_LEGACY_SLOT["'])\s*,\s*(.*?)\s*\)\s*\)$`)

// Recognize the shipped, single-line lease assignments without evaluating any
// Python. Mask strings/comments first so a docstring cannot impersonate code.
// Unknown formatting and expressions fail closed rather than guessing a lease.
func installSlotExpression(source string) (string, error) {
	var quote byte
	triple := false
	classes, methods := 0, 0
	inClass, inMethod := false, false
	assignments := []string{}
	for _, line := range strings.Split(source, "\n") {
		leading := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if strings.ContainsRune(leading, '\t') {
			return "", errors.New("Cannot safely identify the installed queue lease configuration")
		}
		mask := []byte(line)
		comment := len(line)
		for index := 0; index < len(line); {
			if quote != 0 {
				mask[index] = ' '
				if line[index] == '\\' {
					if index+1 < len(line) {
						mask[index+1] = ' '
						index += 2
						continue
					}
					index++
					continue
				}
				if line[index] == quote {
					if triple {
						if index+2 < len(line) && line[index+1] == quote && line[index+2] == quote {
							mask[index+1] = ' '
							mask[index+2] = ' '
							index += 3
							quote = 0
							triple = false
							continue
						}
					} else {
						quote = 0
					}
				}
				index++
				continue
			}
			if line[index] == '#' {
				comment = index
				for rest := index; rest < len(line); rest++ {
					mask[rest] = ' '
				}
				break
			}
			if line[index] == '\'' || line[index] == '"' {
				quote = line[index]
				mask[index] = ' '
				if index+2 < len(line) && line[index+1] == quote && line[index+2] == quote {
					triple = true
					mask[index+1] = ' '
					mask[index+2] = ' '
					index += 3
				} else {
					index++
				}
				continue
			}
			index++
		}
		code := strings.TrimSpace(string(mask))
		if code == "" {
			continue
		}
		indent := len(mask) - len(bytes.TrimLeft(mask, " "))
		if indent == 0 {
			inClass = code == "class Gate:"
			inMethod = false
			if inClass {
				classes++
			}
			continue
		}
		if !inClass {
			continue
		}
		if indent <= 4 {
			inMethod = indent == 4 && code == "def __init__(self):"
			if inMethod {
				methods++
			}
			continue
		}
		if !inMethod {
			continue
		}
		if installSlotAssignment.MatchString(string(mask)) {
			original := strings.TrimSpace(line[:comment])
			_, expression, ok := strings.Cut(original, "=")
			if !ok {
				return "", errors.New("Cannot safely identify the installed queue lease configuration")
			}
			assignments = append(assignments, strings.TrimSpace(expression))
		}
	}
	if quote != 0 || classes != 1 || methods != 1 || len(assignments) != 1 {
		return "", errors.New("Cannot safely identify the installed queue lease configuration")
	}
	return assignments[0], nil
}

func installPythonString(value string) (string, error) {
	if len(value) < 2 || (value[0] != '\'' && value[0] != '"') || value[len(value)-1] != value[0] {
		return "", errors.New("Cannot safely identify the installed queue lease configuration")
	}
	quote := value[0]
	body := value[1 : len(value)-1]
	var result strings.Builder
	for len(body) > 0 {
		character, multi, remaining, err := strconv.UnquoteChar(body, quote)
		if err != nil {
			return "", errors.New("Cannot safely identify the installed queue lease configuration")
		}
		if multi {
			result.WriteRune(character)
		} else {
			result.WriteByte(byte(character))
		}
		body = remaining
	}
	return result.String(), nil
}

func installValidateSlot(raw []byte) (string, error) {
	value, err := decodeObject(raw)
	if err != nil || len(value) != 1 {
		return "", errors.New("Invalid persisted queue lease setting")
	}
	slot, ok := value["slot"].(string)
	if !ok || !filepath.IsAbs(slot) || strings.ContainsRune(slot, 0) {
		return "", errors.New("Invalid persisted queue lease setting")
	}
	return slot, nil
}

func (install *installer) legacySlot() (string, error) {
	setting := filepath.Join(install.state, "legacy-slot.json")
	exists, err := updateExists(setting)
	if err != nil {
		return "", err
	}
	if exists {
		value, err := ReadPrivate(setting)
		if err != nil {
			return "", err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		_, err = installValidateSlot(raw)
		return "", err
	}
	previous := filepath.Join(install.root, "agent_heavy.py")
	exists, err = updateExists(previous)
	if err != nil || !exists {
		return "", err
	}
	raw, err := updateReadOwned(previous, 4*1024*1024)
	if err != nil {
		return "", err
	}
	expression, err := installSlotExpression(string(raw))
	if err != nil {
		return "", err
	}
	if expression == "lease_slot(self.state)" {
		return "", nil
	}
	lookup := installSlotLookup.FindStringSubmatch(expression)
	if len(lookup) != 2 {
		return "", errors.New("Cannot safely identify the installed queue lease configuration")
	}
	if regexp.MustCompile(`^self\.state\s*/\s*["']execution\.slot["']$`).MatchString(lookup[1]) {
		return "", nil
	}
	slot, err := installPythonString(lookup[1])
	if err != nil {
		return "", err
	}
	raw, _ = json.Marshal(Object{"slot": slot})
	return installValidateSlot(raw)
}

func (install *installer) pythonMigration(ctx context.Context, node *yaml.Node) (bool, error) {
	oldSource, err := updateExists(filepath.Join(install.root, "agent_heavy.py"))
	if err != nil {
		return false, err
	}
	callback := ""
	if node != nil {
		callback, err = installConfigCallback(node)
		if err != nil {
			return false, err
		}
	}
	old := oldSource || strings.Contains(callback, "agent_heavy.py") || strings.Contains(callback, ".venv/")
	controllerPath := filepath.Join(install.launch, "io.went.agent-resources.plist")
	if exists, err := updateExists(controllerPath); err != nil {
		return false, err
	} else if exists {
		info, err := updatePlist(controllerPath)
		if err != nil {
			return false, err
		}
		raw, _ := json.Marshal(info["ProgramArguments"])
		if strings.Contains(string(raw), "agent_heavy.py") || strings.Contains(string(raw), ".venv/") {
			old = true
		}
	}
	if !old {
		return false, nil
	}
	if node != nil && !install.standardCleanupCallback(callback) {
		return false, errors.New("Existing queue callback is customized. Preserve its behavior and replace its Python invocation before opening Agent Queue again")
	}
	// A retry may find the new daemon already published and Python files still
	// awaiting retirement. Its active jobs are no longer part of legacy drain.
	if install.embeddedDefinition() {
		return true, nil
	}
	remote := filepath.Join(install.config, "remote.json")
	if exists, err := updateExists(remote); err != nil {
		return false, err
	} else if exists {
		settings, err := ReadPrivate(remote)
		if err != nil {
			return false, err
		}
		enabled, ok := settings["enabled"].(bool)
		if !ok {
			return false, errors.New("Cannot safely inspect remote access configuration")
		}
		if enabled {
			return false, errInstallLegacyPhoneAccess
		}
	}
	if install.loaded(ctx, "io.went.agent-remote") {
		return false, errInstallLegacyPhoneAccess
	}
	loaded := install.loaded(ctx, "io.went.agent-heavy")
	if loaded {
		if exists, err := updateExists(filepath.Join(install.launch, "io.went.agent-heavy.plist")); err != nil {
			return false, err
		} else if !exists {
			return false, errors.New("Cannot establish ownership of the running legacy queue without its launch agent definition")
		}
	}
	if node != nil {
		tasks, err := install.tasks(ctx, node, loaded)
		if err != nil {
			return false, err
		}
		if err := installRequireDrained(tasks); err != nil {
			return false, err
		}
	} else if loaded {
		return false, errors.New("Cannot inspect a running legacy queue without its dedicated configuration")
	}
	return true, install.requireLegacyCleanup()
}

func (install *installer) remoteEnabled() (bool, error) {
	filename := filepath.Join(install.config, "remote.json")
	exists, err := updateExists(filename)
	if err != nil || !exists {
		return false, err
	}
	settings, err := ReadPrivate(filename)
	if err != nil {
		return false, err
	}
	enabled, ok := settings["enabled"].(bool)
	if !ok {
		return false, errors.New("Cannot safely inspect remote access configuration")
	}
	return enabled, nil
}

func (install *installer) disabledRemoteCleanup(ctx context.Context) (bool, error) {
	if install.loaded(ctx, "io.went.agent-remote") {
		return true, nil
	}
	remoteState := filepath.Join(install.state, "remote")
	filename := filepath.Join(remoteState, "processes.json")
	if exists, err := updateExists(filename); err != nil {
		return false, err
	} else if exists {
		record, err := ReadPrivate(filename)
		if err != nil {
			return false, err
		}
		active, ok := record["active"].(bool)
		if !ok {
			return false, errors.New("Cannot safely inspect remote generation ownership")
		}
		if active {
			return true, nil
		}
	}
	parent, err := integrationParent(filepath.Join(remoteState, "children/.installer-probe"), false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer parent.Close()
	entries, err := parent.ReadDir(-1)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			return true, nil
		}
	}
	return false, nil
}

func (install *installer) validatePhoneAccessRecovery(ctx context.Context) error {
	const label = "io.went.agent-remote"
	filename := filepath.Join(install.launch, label+".plist")
	exists, err := updateExists(filename)
	if err != nil {
		return err
	}
	output, queryErr := install.run(ctx, "launchctl", "print", install.domain()+"/"+label)
	if queryErr != nil && !errors.Is(queryErr, errInstallServiceNotLoaded) {
		return fmt.Errorf("Cannot inspect phone access service ownership: %w", queryErr)
	}
	loaded := queryErr == nil
	if !exists {
		if loaded {
			return errors.New("Cannot establish phone access service ownership without its launch agent definition")
		}
		return nil
	}
	definition, err := updatePlist(filename)
	if err != nil {
		return err
	}
	arguments, err := stringsFrom(definition["ProgramArguments"])
	legacy := []string{filepath.Join(install.root, ".venv/bin/python"), filepath.Join(install.root, "remote/control.py"), "serve"}
	legacyRemote := []string{filepath.Join(install.root, "remote/.venv/bin/python"), filepath.Join(install.root, "remote/control.py"), "serve"}
	embedded := []string{filepath.Join(install.root, "bin/agent-remote"), "serve"}
	if err != nil || text(definition["Label"]) != label || (!slices.Equal(arguments, legacy) && !slices.Equal(arguments, legacyRemote) && !slices.Equal(arguments, embedded)) {
		return errors.New("Phone access launch agent is customized; resolve its ownership before continuing setup")
	}
	if program, present := definition["Program"]; present && text(program) != arguments[0] {
		return errors.New("Phone access launch agent Program differs from its expected executable")
	}
	if raw, present := definition["EnvironmentVariables"]; present {
		environment := obj(raw)
		if environment == nil {
			return errors.New("Phone access launch agent environment is invalid")
		}
		for key, expected := range map[string]string{"HOME": install.home, "AGENT_REMOTE_STATE": filepath.Join(install.state, "remote"), "AGENT_REMOTE_CONFIG": filepath.Join(install.config, "remote.json")} {
			if actual, present := environment[key]; present && text(actual) != expected {
				return errors.New("Phone access launch agent targets another installation")
			}
		}
		if value, present := environment["AGENT_REMOTE_TEST_MODE"]; present && text(value) != "0" {
			return errors.New("Phone access launch agent uses a private test environment")
		}
	}
	if loaded {
		if !installLoadedServiceMatches(output, arguments) {
			return errors.New("Loaded phone access service differs from its owned launch agent; no service was stopped")
		}
	}
	return nil
}

// launchctl print exposes the loaded executable and arguments. A disk plist
// alone cannot prove which process a service label currently owns.
func installLoadedServiceMatches(output []byte, expected []string) bool {
	if len(expected) == 0 {
		return false
	}
	return installLoadedServiceProgramMatches(output, expected[0], expected)
}

func installLoadedServiceProgramMatches(output []byte, program string, expected []string) bool {
	programMatches := false
	arguments := []string{}
	inArguments := false
	programs, blocks := 0, 0
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "program = ") {
			programs++
			programMatches = line == "program = "+program
		}
		if line == "arguments = {" {
			blocks++
			inArguments = true
			continue
		}
		if inArguments {
			if line == "}" {
				inArguments = false
			} else {
				arguments = append(arguments, line)
			}
		}
	}
	return programs == 1 && blocks == 1 && !inArguments && programMatches && slices.Equal(arguments, expected)
}

func (install *installer) stopLegacy(ctx context.Context, node *yaml.Node) error {
	stoppedQueue := false
	for _, label := range []string{"io.went.agent-resources", "io.went.agent-heavy"} {
		if install.loaded(ctx, label) {
			definition, err := updatePlist(filepath.Join(install.launch, label+".plist"))
			if err != nil {
				return err
			}
			expected, err := stringsFrom(definition["ProgramArguments"])
			if err != nil || len(expected) == 0 {
				return errors.New("Cannot establish legacy service process ownership")
			}
			loaded, err := install.run(ctx, "launchctl", "print", install.domain()+"/"+label)
			if err != nil {
				return err
			}
			if !installLoadedServiceMatches(loaded, expected) {
				return errors.New("Loaded legacy service differs from its owned launch agent; no process was stopped")
			}
			if label == "io.went.agent-heavy" {
				tasks, err := install.tasks(ctx, node, true)
				if err != nil {
					return err
				}
				if err := installRequireDrained(tasks); err != nil {
					return err
				}
			}
			if _, err := install.run(ctx, "launchctl", "bootout", install.domain()+"/"+label); err != nil {
				return err
			}
			if label == "io.went.agent-heavy" {
				stoppedQueue = true
			}
		}
	}
	if stoppedQueue {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := install.run(ctx, "pueue", "--config", filepath.Join(install.config, "pueue.yml"), "--color", "never", "status", "--json"); err != nil {
				break
			}
			if time.Now().After(deadline) {
				return errors.New("The old queue daemon did not stop; no Python support was replaced")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if node != nil {
		tasks, err := install.tasks(ctx, node, false)
		if err != nil {
			return err
		}
		if err := installRequireDrained(tasks); err != nil {
			return err
		}
		if err := install.requireLegacyCleanup(); err != nil {
			return err
		}
	}
	// Confirm no scheduler instance still holds its kernel lifetime lock.
	deadline := time.Now().Add(12 * time.Second)
	for {
		controller, err := installLock(install.state, "resource-controller.lock")
		if err == nil {
			controller.Close()
			break
		}
		if time.Now().After(deadline) {
			return errors.New("The old resource controller is still stopping; retry after it exits")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	// An independently started Python process is outside launchd ownership.
	// Never kill it or remove its interpreter while it may own work.
	output, err := install.run(ctx, "ps", "-axo", "pid=,command=")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "pueued") && strings.Contains(line, filepath.Join(install.config, "pueue.yml")) {
			return errors.New("An external queue daemon is still running outside its owned launch agent; stop that daemon before retrying installation")
		}
		for _, legacy := range []string{filepath.Join(install.root, ".venv/bin/python"), filepath.Join(install.root, "agent_heavy.py"), filepath.Join(install.root, "remote/control.py")} {
			if strings.Contains(line, legacy) {
				return errors.New("An existing Python queue process is still running. Stop that owned process before migrating; the installer did not remove its runtime")
			}
		}
	}
	return nil
}

func (install *installer) copyFile(source, destination string, mode os.FileMode) error {
	raw, err := install.readPayload(source, updateMaxArchive)
	if err != nil {
		return err
	}
	return installAtomic(destination, raw, mode)
}

func installCopyTree(source, destination string) error {
	return installCopyPayloadTree(source, destination, source, false)
}

func installCopyPayloadTree(source, destination, boundary string, bundled bool) error {
	if err := installDirectory(destination); err != nil {
		return err
	}
	count := 0
	var total int64
	return filepath.WalkDir(source, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 10000 {
			return errors.New("Application payload exceeds its file limit")
		}
		relative, err := filepath.Rel(source, filename)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return installDirectory(target)
		}
		if !entry.Type().IsRegular() {
			return errors.New("Application payload contains a symlink or special file")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if info.Size() > updateMaxArchive || total > updateMaxExpanded {
			return errors.New("Application payload exceeds its size limit")
		}
		raw, err := installReadPayload(boundary, filename, updateMaxArchive, bundled)
		if err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if info.Mode().Perm()&0111 != 0 {
			mode = 0700
		}
		return installAtomic(target, raw, mode)
	})
}

func (install *installer) copyApp(ctx context.Context, source, destination string) error {
	// Validate every entry with the same bounded, no-symlink copier used by
	// isolated tests. macOS then preserves the original app's quarantine, ACLs,
	// resource forks and extended attributes before the atomic app publication.
	if err := installCopyTree(source, destination); err != nil {
		return err
	}
	if install.testing {
		return nil
	}
	// ditto merges an existing root without copying that root's attributes.
	// Remove only our validated staging copy so its native copy inherits them.
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	_, err := install.run(ctx, "/usr/bin/ditto", "--rsrc", "--extattr", "--qtn", "--acl", source, destination)
	return err
}

func (install *installer) support() error {
	for _, directory := range []string{"bin", "scripts"} {
		if err := installDirectory(filepath.Join(install.root, directory)); err != nil {
			return err
		}
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		if err := install.copyFile(filepath.Join(install.payload, "source/bin", name), filepath.Join(install.root, "bin", name), 0700); err != nil {
			return err
		}
		wrapper := []byte("#!/bin/sh\nexec \"$HOME/.local/share/agent-heavy/bin/" + name + "\" \"$@\"\n")
		if err := installAtomic(filepath.Join(install.home, ".local/bin", name), wrapper, 0700); err != nil {
			return err
		}
	}
	for _, name := range []string{"README.md", "LICENSE", "THIRD-PARTY-NOTICES.md", "VERSION", "scripts/install-user.sh"} {
		filename := filepath.Join(install.payload, "source", name)
		exists, err := updateExists(filename)
		if err != nil {
			return err
		}
		if exists {
			mode := os.FileMode(0600)
			if strings.HasSuffix(name, ".sh") {
				mode = 0700
			}
			if err := install.copyFile(filename, filepath.Join(install.root, name), mode); err != nil {
				return err
			}
		}
	}
	licenses := filepath.Join(install.payload, "source/licenses")
	if exists, err := updateExists(licenses); err != nil {
		return err
	} else if exists {
		boundary := install.payload
		if install.appBundle {
			boundary = install.app
		}
		if err := installCopyPayloadTree(licenses, filepath.Join(install.root, "licenses"), boundary, install.appBundle); err != nil {
			return err
		}
	}
	return nil
}

func (install *installer) writeConfig(node *yaml.Node, migration bool) error {
	filename := filepath.Join(install.config, "pueue.yml")
	if node == nil {
		return nil
	}
	if !migration {
		return nil
	}
	callback := installYAMLValue(installYAMLValue(node, "daemon"), "callback")
	if callback == nil {
		return errors.New("Missing queue callback")
	}
	callback.Value = installQuote(filepath.Join(install.root, "bin/agent-heavy")) + " cleanup {{ id }}"
	raw, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	return installAtomic(filename, raw, 0600)
}

func (install *installer) writePlist(filename string, value Object) error {
	raw, err := plist.MarshalIndent(value, plist.XMLFormat, "  ")
	if err != nil {
		return err
	}
	return installAtomic(filename, raw, 0600)
}

func (install *installer) serviceDefinitions(migration bool) error {
	environment := Object{"HOME": install.home, "PATH": ToolPath, "AGENT_HEAVY_STATE": install.state, "AGENT_HEAVY_CONFIG": filepath.Join(install.config, "pueue.yml"), "AGENT_HEAVY_LEGACY_SLOT": install.serviceSlot}
	queue := filepath.Join(install.launch, "io.went.agent-heavy.plist")
	if exists, err := updateExists(queue); err != nil {
		return err
	} else if !exists || migration {
		value := Object{"Label": "io.went.agent-heavy", "ProgramArguments": []string{filepath.Join(install.root, "bin/agent-heavy"), "daemon"}, "EnvironmentVariables": environment, "KeepAlive": Object{"SuccessfulExit": false}, "RunAtLoad": true, "StandardErrorPath": filepath.Join(install.state, "daemon.log"), "StandardOutPath": filepath.Join(install.state, "daemon.log"), "ThrottleInterval": 10, "Umask": 077, "WorkingDirectory": install.state}
		if exists {
			previous, err := updatePlist(queue)
			if err != nil {
				return err
			}
			for key, field := range previous {
				if key != "Program" && key != "ProgramArguments" {
					value[key] = field
				}
			}
		}
		value["EnvironmentVariables"] = install.serviceEnvironment(obj(value["EnvironmentVariables"]))
		if err := install.writePlist(queue, value); err != nil {
			return err
		}
	}
	controller := filepath.Join(install.launch, "io.went.agent-resources.plist")
	if exists, err := updateExists(controller); err != nil {
		return err
	} else if !exists || migration {
		value := Object{"Label": "io.went.agent-resources", "ProgramArguments": []string{filepath.Join(install.root, "bin/agent-heavy"), "scheduler"}, "EnvironmentVariables": environment, "KeepAlive": true, "RunAtLoad": true, "ThrottleInterval": 10, "Umask": 077, "StandardErrorPath": filepath.Join(install.state, "resources.log"), "StandardOutPath": filepath.Join(install.state, "resources.log"), "WorkingDirectory": install.state}
		if exists {
			previous, err := updatePlist(controller)
			if err != nil {
				return err
			}
			for key, field := range previous {
				if key != "Program" && key != "ProgramArguments" {
					value[key] = field
				}
			}
			if value["Label"] != "io.went.agent-resources" {
				return errors.New("Unexpected resource controller service identity")
			}
		}
		value["EnvironmentVariables"] = install.serviceEnvironment(obj(value["EnvironmentVariables"]))
		if err := install.writePlist(controller, value); err != nil {
			return err
		}
	}
	return install.writePlist(filepath.Join(install.launch, "io.went.agent-queue.plist"), Object{"Label": "io.went.agent-queue", "ProgramArguments": []string{"/usr/bin/open", "-a", install.app}, "RunAtLoad": true, "KeepAlive": false})
}

func (install *installer) serviceEnvironment(previous Object) Object {
	environment := Object{}
	for key, value := range previous {
		environment[key] = value
	}
	environment["HOME"] = install.home
	environment["AGENT_HEAVY_STATE"] = install.state
	environment["AGENT_HEAVY_CONFIG"] = filepath.Join(install.config, "pueue.yml")
	environment["AGENT_HEAVY_LEGACY_SLOT"] = install.serviceSlot
	return environment
}

func (install *installer) removeLegacy() error {
	if err := install.validateRetirement(); err != nil {
		return err
	}
	root, err := os.OpenRoot(install.root)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range installLegacyFiles {
		filename := filepath.Join(install.root, name)
		exists, err := updateExists(filename)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		info, err := os.Lstat(filename)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("Unsafe obsolete Python support file; resolve it before retrying installation")
		}
		parent, err := integrationParent(filename, false)
		if err != nil {
			return err
		}
		err = unix.Unlinkat(int(parent.Fd()), filepath.Base(filename), 0)
		parent.Close()
		if err != nil {
			return err
		}
	}
	for _, name := range installLegacyDirectories {
		filename := filepath.Join(install.root, name)
		exists, err := updateExists(filename)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := installDirectory(filename); err != nil {
			return err
		}
		if err := root.RemoveAll(name); err != nil {
			return err
		}
	}
	return nil
}

func (install *installer) publishApp(ctx context.Context) error {
	staging := filepath.Join(install.home, "Applications/.Agent Queue.installing.app")
	previous := filepath.Join(install.home, "Applications/.Agent Queue.previous.app")
	for _, directory := range []string{staging, previous} {
		if exists, err := updateExists(directory); err != nil {
			return err
		} else if exists {
			if err := updatePrivateDirectory(directory); err != nil {
				return err
			}
			if err := os.RemoveAll(directory); err != nil {
				return err
			}
		}
	}
	if err := install.copyApp(ctx, filepath.Join(install.payload, "Agent Queue.app"), staging); err != nil {
		return err
	}
	if !install.testing {
		_, _ = install.run(ctx, "/usr/bin/osascript", "-e", `tell application id "io.went.agent-queue" to quit`)
	}
	appExists, err := updateExists(install.app)
	if err != nil {
		return err
	}
	if appExists {
		if err := installDirectory(install.app); err != nil {
			return err
		}
		if err := os.Rename(install.app, previous); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, install.app); err != nil {
		if appExists {
			_ = os.Rename(previous, install.app)
		}
		return err
	}
	if appExists {
		if err := os.RemoveAll(previous); err != nil {
			return err
		}
	}
	return nil
}

func (install *installer) execute(ctx context.Context) error {
	if !install.testing && runtime.GOOS != "darwin" {
		return errors.New("Agent Job Queue requires macOS")
	}
	if err := install.resolvePayloadApp(); err != nil {
		return err
	}
	if err := install.validatePayload(); err != nil {
		return err
	}
	if install.prepareReplacement || install.unregisterServices {
		if !install.managed || install.prepareReplacement && install.appBundle || install.unregisterServices && !install.appBundle || install.prepareReplacement && install.unregisterServices {
			return errors.New("Replacement preparation requires a marked managed payload; service removal requires a marked installed app bundle")
		}
		return install.prepareManaged(ctx)
	}
	if install.managed {
		return install.executeManaged(ctx)
	}
	if metadata, err := appservices.Load(install.home); err != nil {
		return err
	} else if metadata != nil {
		return errors.New("A managed installation cannot be replaced with an unmarked legacy payload")
	}
	directories := []string{install.root, install.state, install.config, filepath.Join(install.home, ".local/bin"), install.launch}
	if !install.appBundle {
		directories = append(directories, filepath.Join(install.home, "Applications"))
	}
	for _, directory := range directories {
		if err := installDirectory(directory); err != nil {
			return err
		}
	}
	lock, err := installLock(install.state, "install.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	// Recover a crash between moving the previous app aside and publishing the
	// replacement. Never discard the last usable app before its replacement exists.
	previousApp := filepath.Join(install.home, "Applications/.Agent Queue.previous.app")
	if appExists, err := updateExists(install.app); err != nil {
		return err
	} else if !install.appBundle && !appExists {
		if backupExists, err := updateExists(previousApp); err != nil {
			return err
		} else if backupExists {
			if err := installDirectory(previousApp); err != nil {
				return err
			}
			if err := os.Rename(previousApp, install.app); err != nil {
				return err
			}
		}
	}
	// Identity validation repeats under the kernel lock: a delayed older installer
	// must not overtake a newer install that completed while it was starting.
	if err := install.validatePayload(); err != nil {
		return err
	}
	scheduling, err := installLock(install.state, "scheduling.lock")
	if err != nil {
		return err
	}
	defer scheduling.Close()
	var node *yaml.Node
	exists, err := updateExists(filepath.Join(install.config, "pueue.yml"))
	if err != nil {
		return err
	}
	if exists {
		node, err = installConfig(filepath.Join(install.config, "pueue.yml"))
		if err != nil {
			return err
		}
	}
	if err := install.validateServices(); err != nil {
		return err
	}
	if err := install.validateLoadedServices(ctx); err != nil {
		return err
	}
	slot := install.serviceSlot
	migration, err := install.pythonMigration(ctx, node)
	if errors.Is(err, errInstallLegacyPhoneAccess) && install.disablePhoneAccess {
		if err := install.validateRetirement(); err != nil {
			return err
		}
		if err := install.validatePhoneAccessRecovery(ctx); err != nil {
			return err
		}
		// The bundled controller understands legacy process ownership. Never
		// execute the old Python runtime or edit its enabled flag ourselves.
		// Run before taking control.lock, which the controller acquires itself.
		if _, err := install.run(ctx, filepath.Join(install.payload, "source/bin/agent-remote"), "disable"); err != nil {
			return fmt.Errorf("Phone access could not be disabled; setup has not continued: %w", err)
		}
		migration, err = install.pythonMigration(ctx, node)
	}
	if err != nil {
		return err
	}
	if migration {
		if err := install.validateRetirement(); err != nil {
			return err
		}
	}
	queueMigration, err := install.queueMigration(ctx, node)
	if err != nil {
		return err
	}
	remoteState := filepath.Join(install.state, "remote")
	if err := installDirectory(remoteState); err != nil {
		return err
	}
	remoteControl, err := installLock(remoteState, "control.lock")
	if err != nil {
		return fmt.Errorf("A remote settings operation is running; finish it before opening Agent Queue again: %w", err)
	}
	defer remoteControl.Close()
	// Serialize the last legacy/remote snapshot with publication of the stable
	// executable. A waiting old control process can only launch the new binary
	// once this lock is released.
	if currentMigration, err := install.pythonMigration(ctx, node); err != nil {
		return err
	} else if currentMigration != migration {
		return errors.New("Installed queue migration state changed; open Agent Queue again")
	}
	if currentMigration, err := install.queueMigration(ctx, node); err != nil {
		return err
	} else if currentMigration != queueMigration {
		return errors.New("Installed queue migration state changed; open Agent Queue again")
	}
	remoteWasEnabled, err := install.remoteEnabled()
	if err != nil {
		return err
	}
	remoteNeedsCleanup := false
	if !remoteWasEnabled {
		remoteNeedsCleanup, err = install.disabledRemoteCleanup(ctx)
		if err != nil {
			return err
		}
	}
	if remoteWasEnabled || remoteNeedsCleanup {
		if err := install.validatePhoneAccessRecovery(ctx); err != nil {
			return err
		}
	}
	if queueMigration || (migration && !install.embeddedDefinition()) {
		if err := install.stopLegacy(ctx, node); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(install.identity, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := installAtomic(filepath.Join(install.root, "install-in-progress.json"), raw, 0600); err != nil {
		return err
	}
	if queueMigration {
		snapshot, err := install.legacySnapshot(ctx, node, false)
		if err != nil {
			return err
		}
		if err := installRequireDrained(obj(snapshot["tasks"])); err != nil {
			return err
		}
		data, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if err := installAtomic(filepath.Join(install.state, "legacy-queue-import.json"), data, 0600); err != nil {
			return err
		}
	}
	if slot != "" {
		settings, _ := json.Marshal(Object{"slot": slot})
		if err := installAtomic(filepath.Join(install.state, "legacy-slot.json"), settings, 0600); err != nil {
			return err
		}
	}
	if err := install.support(); err != nil {
		return err
	}
	if err := remoteControl.Close(); err != nil {
		return err
	}
	for _, directory := range []string{filepath.Join(install.state, "run")} {
		if err := installDirectory(directory); err != nil {
			return err
		}
	}
	if err := install.writeConfig(node, migration); err != nil {
		return err
	}
	if err := install.serviceDefinitions(migration || queueMigration); err != nil {
		return err
	}
	if !install.loaded(ctx, "io.went.agent-heavy") {
		if _, err := install.run(ctx, "launchctl", "bootstrap", install.domain(), filepath.Join(install.launch, "io.went.agent-heavy.plist")); err != nil {
			return err
		}
	}
	ready := false
	for attempt := 0; attempt < 30; attempt++ {
		if _, err := install.run(ctx, filepath.Join(install.root, "bin/agent-heavy"), "status", "--json"); err == nil {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !ready {
		return errors.New("The queue daemon did not become ready. Inspect the private daemon log")
	}
	if !install.appBundle {
		if err := install.publishApp(ctx); err != nil {
			return err
		}
	}
	if migration {
		if err := install.removeLegacy(); err != nil {
			return err
		}
	}
	if install.loaded(ctx, "io.went.agent-resources") {
		if _, err := install.run(ctx, "launchctl", "kickstart", "-k", install.domain()+"/io.went.agent-resources"); err != nil {
			return err
		}
	} else {
		if _, err := install.run(ctx, "launchctl", "bootstrap", install.domain(), filepath.Join(install.launch, "io.went.agent-resources.plist")); err != nil {
			return err
		}
	}
	if remoteWasEnabled {
		if err := install.validatePhoneAccessRecovery(ctx); err != nil {
			return err
		}
		// The new remote executable reconciles recorded process ownership, stops
		// its old API/gateway/supervisor, and restores enabled access with the
		// existing credentials and private CA. Never leave an external gateway
		// running indefinitely after upgrading to the embedded implementation.
		if _, err := install.run(ctx, filepath.Join(install.root, "bin/agent-remote"), "migrate"); err != nil {
			return fmt.Errorf("Remote access migration did not finish; retry this installer to preserve enabled access: %w", err)
		}
	} else if remoteNeedsCleanup {
		if err := install.validatePhoneAccessRecovery(ctx); err != nil {
			return err
		}
		if _, err := install.run(ctx, filepath.Join(install.root, "bin/agent-remote"), "migrate"); err != nil {
			return fmt.Errorf("Disabled remote service cleanup did not finish; retry this installer: %w", err)
		}
	}
	if err := installAtomic(filepath.Join(install.root, "build-info.json"), raw, 0600); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(install.root, "install-in-progress.json")); err != nil {
		return err
	}
	directory, err := os.Open(install.root)
	if err != nil {
		return err
	}
	err = directory.Sync()
	directory.Close()
	if err != nil {
		return err
	}
	if !install.testing && !install.appBundle {
		if _, err := install.run(ctx, "/usr/bin/open", "-a", install.app); err != nil {
			return err
		}
	}
	return nil
}

func InstallMain(args []string) int {
	options, optionErr := installParseOptions(args)
	if optionErr != nil {
		fmt.Fprintln(os.Stderr, optionErr)
		return 2
	}
	args = options.target
	disablePhoneAccess := false
	disablePhoneAccess = options.disablePhoneAccess
	if len(args) != 2 || (args[0] != "--payload" && args[0] != "--app-bundle") || !filepath.IsAbs(args[1]) {
		fmt.Fprintln(os.Stderr, "Usage: agent-heavy install --payload DIRECTORY | --app-bundle ABSOLUTE_APP_PATH [--disable-phone-access]")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil || !integrationValidPath(home) {
		fmt.Fprintln(os.Stderr, "Installer requires an absolute user home directory")
		return 1
	}
	path := ToolPath
	testing := os.Getenv("AGENT_QUEUE_INSTALL_TEST_MODE") == "1"
	if testing {
		tools := os.Getenv("AGENT_QUEUE_INSTALL_TEST_TOOLS")
		if !integrationValidPath(tools) {
			fmt.Fprintln(os.Stderr, "Installer test tools must name an absolute directory")
			return 1
		}
		path = tools + string(os.PathListSeparator) + path
	}
	install := &installer{home: home, payload: args[1], root: filepath.Join(home, ".local/share/agent-heavy"), state: filepath.Join(home, ".local/state/agent-heavy"), config: filepath.Join(home, ".config/agent-heavy"), launch: filepath.Join(home, "Library/LaunchAgents"), app: filepath.Join(home, "Applications/Agent Queue.app"), path: path, testing: testing, disablePhoneAccess: disablePhoneAccess}
	install.prepareReplacement = options.prepareReplacement
	install.unregisterServices = options.unregisterServices
	if args[0] == "--app-bundle" {
		install.appBundle = true
		install.app = args[1]
		install.payload = filepath.Join(install.app, "Contents/Resources/payload")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if options.repairAndContinue {
		requests := make(chan os.Signal, 1)
		signal.Notify(requests, syscall.SIGUSR1)
		defer signal.Stop(requests)
		requested, done := make(chan struct{}), make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-requests:
				close(requested)
			case <-done:
			}
		}()
		install.setupStop = requested
	}
	if !options.repairAndContinue {
		bounded, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		ctx = bounded
	}
	if options.preflight || options.recovery != "" {
		if options.recovery != "" {
			if err := install.recover(ctx, options.recovery, options.jobID, options.revision); err != nil {
				fmt.Fprintf(os.Stderr, "Agent Queue recovery: %v\n", err)
				return 1
			}
		}
		if err := installWritePreflight(install.preflight(ctx)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if options.repairAndContinue {
		err = install.repairAndExecute(ctx)
	} else {
		err = install.execute(ctx)
	}
	if err != nil {
		var approval *installApprovalError
		if errors.As(err, &approval) {
			// The app waits for macOS approval, then runs setup again.
			fmt.Fprintln(os.Stderr, "AGENT_QUEUE_SETUP_STATE:requires-approval")
		}
		fmt.Fprintf(os.Stderr, "Agent Queue setup: %v\n", err)
		if errors.Is(err, errInstallLegacyPhoneAccess) {
			fmt.Fprintln(os.Stderr, installDisablePhoneAccessRecovery)
		}
		return 1
	}
	if options.prepareReplacement {
		fmt.Println("Prepared Agent Queue replacement. Services are stopped; the application and CLI wrappers are unchanged.")
		return 0
	}
	if options.unregisterServices {
		fmt.Println("Unregistered Agent Queue services. The application can now be removed; private data is preserved.")
		return 0
	}
	fmt.Println("Installed Agent Queue. Your queue is private; manage optional phone access in settings.")
	fmt.Printf("CLI: %s\n", filepath.Join(home, ".local/bin/agent-heavy"))
	return 0
}
