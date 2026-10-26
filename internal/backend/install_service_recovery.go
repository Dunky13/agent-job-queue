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
	"unicode"

	"golang.org/x/sys/unix"
	"howett.net/plist"
)

const installRestoreServices = "restore-service-definitions"

var installBackgroundLabels = []string{"io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-remote"}

type installMissingService struct {
	label      string
	definition Object
}

// launchctl's print format is not a plist. Recover only its unambiguous direct
// fields; a nested diagnostic must never impersonate the service definition.
func (install *installer) missingServiceDefinition(label string, output []byte) (Object, error) {
	unsafe := errors.New("Cannot restore a missing launch agent: loaded service ownership is incomplete, customized or differs from this installation")
	if len(output) > 128*1024 {
		return nil, unsafe
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != install.domain()+"/"+label+" = {" {
		return nil, unsafe
	}
	stack := []string{"service"}
	values := map[string]string{}
	arguments := []string{}
	environment := Object{}
	blocks := map[string]bool{}
	for _, raw := range lines[1:] {
		line := strings.TrimLeft(raw, "\t")
		if strings.TrimSpace(line) == "" {
			if len(stack) == 2 && stack[1] == "arguments" {
				return nil, unsafe
			}
			continue
		}
		// Values are literal. Trimming trailing whitespace could turn a different
		// command, path or environment value into an accepted canonical one.
		if strings.TrimRightFunc(line, unicode.IsSpace) != line {
			return nil, unsafe
		}
		if len(stack) == 0 {
			return nil, unsafe
		}
		if line == "}" {
			stack = stack[:len(stack)-1]
			continue
		}
		if strings.HasSuffix(line, " = {") {
			if len(stack) >= 2 && (stack[1] == "arguments" || stack[1] == "environment" || stack[1] == "inherited environment" || stack[1] == "default environment") {
				return nil, unsafe
			}
			name := strings.TrimSuffix(line, " = {")
			if len(stack) == 1 && (name == "arguments" || name == "environment" || name == "inherited environment" || name == "default environment") {
				if blocks[name] {
					return nil, unsafe
				}
				blocks[name] = true
			}
			stack = append(stack, name)
			continue
		}
		if len(stack) == 1 {
			for _, key := range []string{"path", "type", "program"} {
				if strings.HasPrefix(line, key+" = ") {
					if _, ok := values[key]; ok {
						return nil, unsafe
					}
					values[key] = strings.TrimPrefix(line, key+" = ")
				}
			}
		} else if len(stack) == 2 && stack[1] == "arguments" {
			// launchctl indents direct arguments with two tabs. Keep every byte
			// after that structural prefix so literal spaces or tabs cannot be
			// normalized into a different accepted command.
			argument, indented := strings.CutPrefix(raw, "\t\t")
			if !indented || strings.ContainsAny(argument, "{}") || strings.IndexFunc(argument, unicode.IsControl) >= 0 {
				return nil, unsafe
			}
			arguments = append(arguments, argument)
		} else if len(stack) == 2 && (stack[1] == "environment" || stack[1] == "inherited environment" || stack[1] == "default environment") {
			entry, indented := strings.CutPrefix(raw, "\t\t")
			key, value, ok := strings.Cut(entry, " => ")
			if !indented || !ok || key == "" || strings.IndexFunc(key, unicode.IsControl) >= 0 || strings.ContainsAny(value, "{}") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
				return nil, unsafe
			}
			// Default launchd PATH is superseded by the explicit service environment.
			if stack[1] == "default environment" && key == "PATH" {
				continue
			}
			if stack[1] == "inherited environment" && key == "SSH_AUTH_SOCK" && installLaunchdSocket(value) {
				continue // launchd's inherited socket is not part of the restored definition.
			}
			if err := install.validateRestoredEnvironment(label, key, value); err != nil {
				return nil, err
			}
			if stack[1] == "environment" {
				if _, exists := environment[key]; exists {
					return nil, unsafe
				}
				if key != "XPC_SERVICE_NAME" && key != "OSLogRateLimit" {
					environment[key] = value
				}
			}
		} else if len(stack) > 2 && (stack[1] == "arguments" || stack[1] == "environment") {
			return nil, unsafe
		}
	}
	if len(stack) != 0 || values["path"] != filepath.Join(install.launch, label+".plist") || values["type"] != "LaunchAgent" || !blocks["arguments"] || !blocks["environment"] || len(arguments) == 0 || values["program"] != arguments[0] || text(environment["HOME"]) != install.home || text(environment["PATH"]) == "" {
		return nil, unsafe
	}
	valid := false
	switch label {
	case "io.went.agent-heavy":
		valid = slices.Equal(arguments, []string{filepath.Join(install.root, "bin/agent-heavy"), "daemon"}) || len(arguments) == 3 && slices.Contains([]string{"/opt/homebrew/bin/pueued", "/usr/local/bin/pueued"}, arguments[0]) && slices.Contains([]string{"--config", "-c"}, arguments[1]) && arguments[2] == filepath.Join(install.config, "pueue.yml")
	case "io.went.agent-resources":
		valid = slices.Equal(arguments, []string{filepath.Join(install.root, "bin/agent-heavy"), "scheduler"}) || slices.Equal(arguments, []string{filepath.Join(install.root, ".venv/bin/python"), filepath.Join(install.root, "agent_heavy.py"), "scheduler"})
	case "io.went.agent-remote":
		valid = slices.Equal(arguments, []string{filepath.Join(install.root, "bin/agent-remote"), "serve"}) || slices.Equal(arguments, []string{filepath.Join(install.root, ".venv/bin/python"), filepath.Join(install.root, "remote/control.py"), "serve"}) || slices.Equal(arguments, []string{filepath.Join(install.root, "remote/.venv/bin/python"), filepath.Join(install.root, "remote/control.py"), "serve"})
	}
	if !valid {
		return nil, unsafe
	}
	log := map[string]string{"io.went.agent-heavy": "daemon.log", "io.went.agent-resources": "resources.log", "io.went.agent-remote": "remote/service.log"}[label]
	definition := Object{"Label": label, "ProgramArguments": arguments, "EnvironmentVariables": environment, "RunAtLoad": true, "KeepAlive": true, "ThrottleInterval": 10, "Umask": 077, "StandardOutPath": filepath.Join(install.state, log), "StandardErrorPath": filepath.Join(install.state, log)}
	if label != "io.went.agent-resources" {
		definition["KeepAlive"] = Object{"SuccessfulExit": false}
	}
	if label != "io.went.agent-remote" {
		definition["WorkingDirectory"] = install.state
	}
	return definition, nil
}

func installLaunchdSocket(value string) bool {
	if !integrationValidPath(value) || filepath.Base(value) != "Listeners" {
		return false
	}
	parent := filepath.Dir(value)
	name := filepath.Base(parent)
	return strings.HasPrefix(name, "com.apple.launchd.") && len(name) > len("com.apple.launchd.") && slices.Contains([]string{"/private/tmp", "/var/run", "/private/var/run"}, filepath.Dir(parent))
}

func (install *installer) validateRestoredEnvironment(label, key, value string) error {
	expected := map[string]string{"HOME": install.home, "AGENT_HEAVY_STATE": install.state, "AGENT_HEAVY_CONFIG": filepath.Join(install.config, "pueue.yml"), "AGENT_REMOTE_STATE": filepath.Join(install.state, "remote"), "AGENT_REMOTE_CONFIG": filepath.Join(install.config, "remote.json"), "AGENT_REMOTE_TEST_MODE": "0", "XPC_SERVICE_NAME": label}
	if wanted, ok := expected[key]; ok && value == wanted {
		return nil
	}
	if key == "PATH" {
		for _, directory := range filepath.SplitList(value) {
			if !slices.Contains([]string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}, directory) {
				return errors.New("Loaded service PATH is customized; restore its reviewed definition manually")
			}
		}
		if value != "" {
			return nil
		}
	}
	if key == "OSLogRateLimit" && value == "64" {
		return nil
	}
	if key == "AGENT_HEAVY_LEGACY_SLOT" {
		slot, err := install.configuredSlot()
		if err == nil && value == slot {
			return nil
		}
	}
	return fmt.Errorf("Loaded service environment %s cannot establish this installation's ownership; restore its reviewed definition manually", installDisplayText(key, 80, false))
}

func (install *installer) missingServiceDefinitions(ctx context.Context) ([]installMissingService, error) {
	missing := []installMissingService{}
	if err := install.validateServices(); err != nil {
		return nil, err
	}
	for _, label := range installBackgroundLabels {
		output, loaded, err := install.inspectService(ctx, label)
		if err != nil {
			return nil, err
		}
		filename := filepath.Join(install.launch, label+".plist")
		exists, err := updateExists(filename)
		if err != nil {
			return nil, err
		}
		if !loaded {
			continue
		}
		if exists {
			definition, err := updatePlist(filename)
			if err != nil {
				return nil, err
			}
			args, err := stringsFrom(definition["ProgramArguments"])
			if err != nil || !installLoadedServiceMatches(output, args) {
				return nil, errors.New("Loaded service differs from its existing definition")
			}
			if label == "io.went.agent-remote" {
				if err := install.validatePhoneAccessRecovery(ctx); err != nil {
					return nil, err
				}
			}
			continue
		}
		definition, err := install.missingServiceDefinition(label, output)
		if err != nil {
			return nil, err
		}
		if label == "io.went.agent-remote" {
			parent, err := integrationParent(filepath.Join(install.state, "remote/.restore-probe"), false)
			if err != nil {
				return nil, fmt.Errorf("Cannot restore phone service without its safe private state directory: %w", err)
			}
			if err := parent.Close(); err != nil {
				return nil, err
			}
		}
		missing = append(missing, installMissingService{label: label, definition: definition})
	}
	if len(missing) > 0 {
		parent, err := integrationParent(filepath.Join(install.launch, ".restore-probe"), false)
		if err != nil {
			return nil, fmt.Errorf("Cannot restore service files without their safe LaunchAgents directory: %w", err)
		}
		if err := parent.Close(); err != nil {
			return nil, err
		}
	}
	return missing, nil
}

func (install *installer) restoreServiceDefinitions(ctx context.Context) (resultErr error) {
	// The caller already owns install.lock and scheduling.lock. Serialize remote
	// settings too when that subsystem exists; never create a directory in preflight.
	remote := filepath.Join(install.state, "remote")
	if exists, err := updateExists(remote); err != nil {
		return err
	} else if exists {
		if err := installInspectDirectory(remote); err != nil {
			return err
		}
		control, err := installLock(remote, "control.lock")
		if err != nil {
			return err
		}
		defer control.Close()
	} else {
		_, loaded, err := install.inspectService(ctx, "io.went.agent-remote")
		if err != nil {
			return err
		}
		if loaded {
			return errors.New("Cannot restore phone service without its private state directory")
		}
	}
	missing, err := install.missingServiceDefinitions(ctx)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return errors.New("No verified loaded service has a missing definition; check setup again")
	}
	parent, err := integrationParent(filepath.Join(install.launch, ".restore-probe"), false)
	if err != nil {
		return err
	}
	defer parent.Close()
	// Validate the entire set before publishing any definition. O_EXCL prevents
	// replacing a newly appeared file or symlink even after this last check.
	for _, service := range missing {
		var info unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), service.label+".plist", &info, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
			return errors.New("A service definition appeared during recovery; check setup again")
		}
	}
	type createdDefinition struct {
		name     string
		identity unix.Stat_t
	}
	created := []createdDefinition{}
	defer func() {
		if resultErr == nil {
			return
		}
		for _, file := range created {
			var current unix.Stat_t
			if unix.Fstatat(int(parent.Fd()), file.name, &current, unix.AT_SYMLINK_NOFOLLOW) == nil && current.Dev == file.identity.Dev && current.Ino == file.identity.Ino {
				if err := unix.Unlinkat(int(parent.Fd()), file.name, 0); err != nil {
					resultErr = errors.Join(resultErr, err)
				}
			}
		}
		if err := parent.Sync(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	for _, service := range missing {
		output, loaded, err := install.inspectService(ctx, service.label)
		if err != nil {
			return err
		}
		if !loaded {
			return errors.New("A service stopped during recovery; check setup again")
		}
		current, err := install.missingServiceDefinition(service.label, output)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, service.definition) {
			return errors.New("Loaded service changed during recovery; check setup again")
		}
		raw, err := plist.MarshalIndent(service.definition, plist.XMLFormat, "  ")
		if err != nil {
			return err
		}
		name := service.label + ".plist"
		fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), name)
		var info unix.Stat_t
		if err := unix.Fstat(fd, &info); err != nil {
			file.Close()
			return err
		}
		created = append(created, createdDefinition{name: name, identity: info})
		if err := unix.Fchmod(fd, 0600); err != nil {
			file.Close()
			return err
		}
		_, writeErr := file.Write(raw)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return err
		}
	}
	return parent.Sync()
}
