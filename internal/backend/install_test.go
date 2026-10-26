package backend

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"howett.net/plist"
)

type installFixture struct {
	installer *installer
	tasks     Object
	groups    Object
	loaded    map[string]bool
	calls     [][]string
	fail      string
}

func installTestFixture(t *testing.T) *installFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "User With Spaces")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for _, key := range overrides {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	payload := filepath.Join(base, "payload")
	tools := filepath.Join(base, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"brew", "pueue", "pueued"} {
		filename := filepath.Join(tools, tool)
		integrationTestWrite(t, filename, []byte("#!/bin/sh\nexit 0\n"))
		if err := os.Chmod(filename, 0700); err != nil {
			t.Fatal(err)
		}
	}
	info := metadataTestInfo(t, "123")
	updateTestBundle(t, filepath.Join(payload, "Agent Queue.app"), "io.went.agent-queue", info, plist.BinaryFormat)
	integrationTestWrite(t, filepath.Join(payload, "Agent Queue.app/Contents/MacOS/AgentQueue"), []byte("prebuilt app"))
	if err := os.Chmod(filepath.Join(payload, "Agent Queue.app/Contents/MacOS/AgentQueue"), 0700); err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(payload, "source/build-info.json"), mustMetadataJSON(t, info))
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		filename := filepath.Join(payload, "source/bin", name)
		integrationTestWrite(t, filename, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"))
		if err := os.Chmod(filename, 0700); err != nil {
			t.Fatal(err)
		}
	}
	integrationTestWrite(t, filepath.Join(payload, "source/VERSION"), []byte("0.1.0\n"))
	install := &installer{home: home, payload: payload, root: filepath.Join(home, ".local/share/agent-heavy"), state: filepath.Join(home, ".local/state/agent-heavy"), config: filepath.Join(home, ".config/agent-heavy"), launch: filepath.Join(home, "Library/LaunchAgents"), app: filepath.Join(home, "Applications/Agent Queue.app"), path: tools, testing: true}
	f := &installFixture{installer: install, tasks: Object{}, groups: Object{"default": Object{"parallel_tasks": 1, "status": "Running"}}, loaded: map[string]bool{}}
	install.command = func(_ context.Context, args []string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{}, args...))
		joined := strings.Join(args, " ")
		if f.fail != "" && strings.Contains(joined, f.fail) {
			return nil, errors.New("simulated installer failure")
		}
		if args[0] == "launchctl" {
			switch args[1] {
			case "print":
				label := filepath.Base(args[2])
				if f.loaded[label] {
					definition, err := updatePlist(filepath.Join(install.launch, label+".plist"))
					if err != nil {
						return []byte("unowned service"), nil
					}
					arguments, err := stringsFrom(definition["ProgramArguments"])
					if err != nil || len(arguments) == 0 {
						t.Fatal("invalid fixture service")
					}
					program := arguments[0]
					if explicit, present := definition["Program"]; present {
						program = text(explicit)
					}
					return []byte("program = " + program + "\narguments = {\n" + strings.Join(arguments, "\n") + "\n}\n"), nil
				}
				return nil, errInstallServiceNotLoaded
			case "bootstrap":
				label := strings.TrimSuffix(filepath.Base(args[3]), ".plist")
				f.loaded[label] = true
			case "bootout":
				if filepath.Base(args[2]) == "io.went.agent-heavy" {
					integrationTestWrite(t, filepath.Join(install.state, "pueue/state.json"), mustMetadataJSON(t, f.snapshot()))
				}
				delete(f.loaded, filepath.Base(args[2]))
			case "kickstart":
				if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); err != nil {
					t.Fatal("controller restart must happen before completion")
				}
			}
		}
		if args[0] == "pueue" && strings.Contains(joined, "status --json") {
			if !f.loaded["io.went.agent-heavy"] {
				return nil, errors.New("fixture daemon is not running")
			}
			return mustMetadataJSON(t, f.snapshot()), nil
		}
		if args[0] == filepath.Join(install.root, "bin/agent-heavy") && strings.Contains(joined, "status --json") {
			if !f.loaded["io.went.agent-heavy"] {
				return nil, errors.New("fixture embedded daemon is not running")
			}
			return mustMetadataJSON(t, f.snapshot()), nil
		}
		return nil, nil
	}
	return f
}

func (f *installFixture) snapshot() Object {
	return Object{"tasks": f.tasks, "groups": f.groups}
}

func (f *installFixture) run(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return f.installer.execute(ctx)
}

func (install *installer) freshConfig() Object {
	return Object{"shared": Object{"pueue_directory": filepath.Join(install.state, "pueue"), "runtime_directory": filepath.Join(install.state, "run"), "use_unix_socket": true, "unix_socket_permissions": 0600},
		"daemon": Object{"pause_group_on_failure": false, "pause_all_on_failure": false, "compress_state_file": false, "callback": installQuote(filepath.Join(install.root, "bin/agent-heavy")) + " cleanup {{ id }}", "shell_command": []string{"/bin/sh", "-c", "{{ pueue_command_string }}"}},
		"client": Object{"show_confirmation_questions": false, "read_local_logs": true, "edit_mode": "files"}}
}

func (f *installFixture) legacy(t *testing.T) {
	t.Helper()
	install := f.installer
	for _, directory := range []string{install.root, install.state, install.config, install.launch, filepath.Join(install.root, ".venv/bin")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	integrationTestWrite(t, filepath.Join(install.root, "agent_heavy.py"), []byte("raise RuntimeError('must not execute')\nclass Gate:\n    def __init__(self):\n        self.slot = lease_slot(self.state)\n"))
	integrationTestWrite(t, filepath.Join(install.root, ".venv/bin/python"), []byte("old runtime"))
	configuration := install.freshConfig()
	configuration["daemon"].(Object)["callback"] = installQuote(filepath.Join(install.root, ".venv/bin/python")) + " " + installQuote(filepath.Join(install.root, "agent_heavy.py")) + " cleanup {{ id }}"
	configuration["client"].(Object)["custom-option"] = "preserve me"
	integrationTestWrite(t, filepath.Join(install.config, "pueue.yml"), mustMetadataJSON(t, configuration))
	controller := Object{"Label": "io.went.agent-resources", "ProgramArguments": []string{filepath.Join(install.root, ".venv/bin/python"), filepath.Join(install.root, "agent_heavy.py"), "scheduler"}, "EnvironmentVariables": Object{"CUSTOM_OPERATOR_VALUE": "retained"}, "KeepAlive": true}
	data, err := plist.Marshal(controller, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(install.launch, "io.went.agent-resources.plist"), data)
	queue := Object{"Label": "io.went.agent-heavy", "ProgramArguments": []string{filepath.Join(install.path, "pueued"), "--config", filepath.Join(install.config, "pueue.yml")}}
	data, err = plist.Marshal(queue, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(install.launch, "io.went.agent-heavy.plist"), data)
	f.loaded["io.went.agent-heavy"] = true
	f.loaded["io.went.agent-resources"] = true
}

func TestInstallerFreshPythonFreeAndPrivate(t *testing.T) {
	f := installTestFixture(t)
	install := f.installer
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	installed, err := updateInstalled(install.root, install.app)
	if err != nil || installed["build"] != 123 {
		t.Fatalf("installed identity %v %v", installed, err)
	}
	if _, err := os.Stat(filepath.Join(install.config, "pueue.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fresh embedded install generated legacy queue configuration")
	}
	controller, err := updatePlist(filepath.Join(install.launch, "io.went.agent-resources.plist"))
	if err != nil {
		t.Fatal(err)
	}
	args := controller["ProgramArguments"].([]any)
	if len(args) != 2 || args[0] != filepath.Join(install.root, "bin/agent-heavy") || args[1] != "scheduler" {
		t.Fatalf("Go controller %v", args)
	}
	for _, filename := range []string{filepath.Join(install.root, "build-info.json"), filepath.Join(install.launch, "io.went.agent-heavy.plist")} {
		info, err := os.Stat(filename)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file %s: %v %v", filename, info, err)
		}
	}
	for _, filename := range []string{filepath.Join(install.root, ".venv"), filepath.Join(install.config, "remote.json"), filepath.Join(install.root, "install-in-progress.json")} {
		if _, err := os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected file %s", filename)
		}
	}
	for _, args := range f.calls {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "uv") || strings.Contains(joined, "python") || filepath.Base(args[0]) == "caddy" || (filepath.Base(args[0]) == "brew" && len(args) > 1 && args[1] == "install") {
			t.Fatalf("unexpected prerequisite %v", args)
		}
	}
	output, err := exec.Command(filepath.Join(install.home, ".local/bin/agent-heavy"), "argument with spaces", "literal;$HOME").Output()
	if err != nil || string(output) != "argument with spaces\nliteral;$HOME\n" {
		t.Fatalf("wrapper preserved argv: %q %v", output, err)
	}
}

func TestInstallerNeedsNoExternalRuntimeTools(t *testing.T) {
	f := installTestFixture(t)
	install := f.installer
	for _, name := range []string{"brew", "pueue", "pueued"} {
		if err := os.Remove(filepath.Join(install.path, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	f.tasks = Object{"17": Object{"status": Object{"Running": Object{}}, "command": "embedded fixture work"}}
	if err := f.run(t); err != nil {
		t.Fatalf("embedded upgrade requires an external runtime: %v", err)
	}
	for _, args := range f.calls {
		if slices.Contains([]string{"brew", "pueue", "pueued", "caddy"}, filepath.Base(args[0])) {
			t.Fatalf("unexpected runtime prerequisite: %v", args)
		}
	}
	queue, err := updatePlist(filepath.Join(install.launch, "io.went.agent-heavy.plist"))
	if err != nil {
		t.Fatal(err)
	}
	args, err := stringsFrom(queue["ProgramArguments"])
	if err != nil || !slices.Equal(args, []string{filepath.Join(install.root, "bin/agent-heavy"), "daemon"}) {
		t.Fatalf("queue does not use installed embedded daemon: %v %v", args, err)
	}
}

func TestInstallerEnabledRemoteMigratesBeforeCompletionAndRetainsFailureMarker(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	install := f.installer
	credentials := []byte(`{"enabled":true,"password_hash":"preserved password hash","secret_key":"preserved secret"}`)
	configPath := filepath.Join(install.config, "remote.json")
	integrationTestWrite(t, configPath, credentials)
	certificate := filepath.Join(install.state, "remote/caddy/pki/authorities/local/root.crt")
	integrationTestWrite(t, certificate, []byte("preserved private CA fixture"))
	previous := integrationTestRead(t, filepath.Join(install.root, "build-info.json"))
	identity := metadataTestInfo(t, "124")
	integrationTestWrite(t, filepath.Join(install.payload, "source/build-info.json"), mustMetadataJSON(t, identity))
	updateTestBundle(t, filepath.Join(install.payload, "Agent Queue.app"), "io.went.agent-queue", identity, plist.XMLFormat)
	// Active Go queue work stays with its existing daemon during remote migration.
	f.tasks = Object{"3": Object{"status": Object{"Running": Object{}}, "command": "existing Go job"}}
	command := install.command
	migrations := 0
	failMigration := true
	install.command = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == filepath.Join(install.root, "bin/agent-remote") {
			if len(args) != 2 || args[1] != "migrate" {
				t.Fatalf("unexpected remote migration: %v", args)
			}
			migrations++
			if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); err != nil {
				t.Fatal("remote migration must complete before installation marker")
			}
			if !bytes.Equal(integrationTestRead(t, configPath), credentials) || string(integrationTestRead(t, certificate)) != "preserved private CA fixture" {
				t.Fatal("remote credentials or CA changed before lifecycle reconciliation")
			}
			if failMigration {
				return nil, errors.New("fixture ownership reconciliation failure")
			}
		}
		return command(ctx, args)
	}
	if err := f.run(t); err == nil || !strings.Contains(err.Error(), "Remote access migration did not finish") {
		t.Fatalf("failed migration reported complete: %v", err)
	}
	if !bytes.Equal(previous, integrationTestRead(t, filepath.Join(install.root, "build-info.json"))) {
		t.Fatal("failed migration advanced completed identity")
	}
	if _, err := updateInstalled(install.root, install.app); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("failed migration lost retry marker: %v", err)
	}
	failMigration = false
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if migrations != 2 {
		t.Fatalf("migration was not retried: %d", migrations)
	}
	installed, err := updateInstalled(install.root, install.app)
	if err != nil || installed["build"] != 124 {
		t.Fatalf("migration retry did not complete: %v %v", installed, err)
	}
	for _, args := range f.calls {
		if len(args) > 1 && args[0] == "launchctl" && args[1] == "bootout" {
			t.Fatalf("Go migration stopped queue work: %v", args)
		}
	}
	if !bytes.Equal(integrationTestRead(t, configPath), credentials) || string(integrationTestRead(t, certificate)) != "preserved private CA fixture" {
		t.Fatal("remote migration changed credentials or CA")
	}
}

func TestInstallerDisabledRemoteStaysDisabled(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	install := f.installer
	credentials := []byte(`{"enabled":false,"password_hash":"preserved password hash","secret_key":"preserved secret"}`)
	path := filepath.Join(install.config, "remote.json")
	integrationTestWrite(t, path, credentials)
	f.calls = nil
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	for _, args := range f.calls {
		if filepath.Base(args[0]) == "agent-remote" {
			t.Fatalf("disabled access was activated during upgrade: %v", args)
		}
	}
	if !bytes.Equal(integrationTestRead(t, path), credentials) {
		t.Fatal("disabled access credentials changed")
	}
}

func TestInstallerDisabledRemoteReconcilesStaleOwnedServicesBeforeCompletion(t *testing.T) {
	for _, state := range []string{"loaded", "active-generation", "legacy-child"} {
		t.Run(state, func(t *testing.T) {
			f := installTestFixture(t)
			if err := f.run(t); err != nil {
				t.Fatal(err)
			}
			install := f.installer
			credentials := []byte(`{"enabled":false,"password_hash":"preserved password hash","secret_key":"preserved secret"}`)
			configPath := filepath.Join(install.config, "remote.json")
			integrationTestWrite(t, configPath, credentials)
			certificate := filepath.Join(install.state, "remote/caddy/pki/authorities/local/root.crt")
			integrationTestWrite(t, certificate, []byte("preserved private CA fixture"))
			switch state {
			case "loaded":
				f.loaded["io.went.agent-remote"] = true
				definition, err := plist.Marshal(Object{"Label": "io.went.agent-remote", "ProgramArguments": []string{filepath.Join(install.root, "bin/agent-remote"), "serve"}}, plist.XMLFormat)
				if err != nil {
					t.Fatal(err)
				}
				integrationTestWrite(t, filepath.Join(install.launch, "io.went.agent-remote.plist"), definition)
			case "active-generation":
				integrationTestWrite(t, filepath.Join(install.state, "remote/processes.json"), []byte(`{"active":true}`))
			case "legacy-child":
				integrationTestWrite(t, filepath.Join(install.state, "remote/children", strings.Repeat("a", 32)+"-caddy.json"), []byte(`{"role":"caddy","pid":123,"birth":"fixture"}`))
			}
			command := install.command
			cleanups := 0
			failCleanup := true
			install.command = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == filepath.Join(install.root, "bin/agent-remote") {
					if len(args) != 2 || args[1] != "migrate" {
						t.Fatalf("disabled service was reactivated: %v", args)
					}
					cleanups++
					if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); err != nil {
						t.Fatal("stale service cleanup must precede completion marker")
					}
					if failCleanup {
						return nil, errors.New("fixture unverified process ownership")
					}
				}
				return command(ctx, args)
			}
			if err := f.run(t); err == nil || !strings.Contains(err.Error(), "Disabled remote service cleanup did not finish") {
				t.Fatalf("stale service was reported retired: %v", err)
			}
			if _, err := updateInstalled(install.root, install.app); err == nil || !strings.Contains(err.Error(), "incomplete") {
				t.Fatalf("failed stale cleanup lost retry marker: %v", err)
			}
			failCleanup = false
			if err := f.run(t); err != nil {
				t.Fatal(err)
			}
			if cleanups != 2 {
				t.Fatalf("stale ownership cleanup was not retried: %d", cleanups)
			}
			if !bytes.Equal(credentials, integrationTestRead(t, configPath)) || string(integrationTestRead(t, certificate)) != "preserved private CA fixture" {
				t.Fatal("stale cleanup changed credentials or CA")
			}
		})
	}
}

func TestInstallerRemoteMigrationRespectsConcurrentDisableAndControlLock(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	install := f.installer
	config := filepath.Join(install.config, "remote.json")
	enabled := []byte(`{"enabled":true,"secret_key":"preserved fixture"}`)
	disabled := []byte(`{"enabled":false,"secret_key":"preserved fixture"}`)
	integrationTestWrite(t, config, enabled)
	remoteState := filepath.Join(install.state, "remote")
	lock, err := installLock(remoteState, "control.lock")
	if err != nil {
		t.Fatal(err)
	}
	err = f.run(t)
	lock.Close()
	if err == nil || !strings.Contains(err.Error(), "remote settings operation") {
		t.Fatalf("installer did not serialize with remote control: %v", err)
	}
	if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("remote lock contention changed installation")
	}
	command := install.command
	changed, migrated := false, false
	install.command = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "launchctl" && len(args) > 2 && args[1] == "print" && strings.HasSuffix(args[2], "/io.went.agent-heavy") && !changed {
			if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); err == nil {
				control, err := installLock(remoteState, "control.lock")
				if err != nil {
					t.Fatalf("installer retained remote lock after stable binary publication: %v", err)
				}
				integrationTestWrite(t, config, disabled)
				control.Close()
				changed = true
			}
		}
		if args[0] == filepath.Join(install.root, "bin/agent-remote") {
			if len(args) != 2 || args[1] != "migrate" {
				t.Fatalf("snapshot action could undo latest user choice: %v", args)
			}
			if !changed || !bytes.Equal(integrationTestRead(t, config), disabled) {
				t.Fatal("migrate did not receive the latest disabled choice")
			}
			control, err := installLock(remoteState, "control.lock")
			if err != nil {
				t.Fatalf("installer would deadlock remote migration: %v", err)
			}
			control.Close()
			migrated = true
		}
		return command(ctx, args)
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if !migrated || !bytes.Equal(integrationTestRead(t, config), disabled) {
		t.Fatal("concurrent disable was lost")
	}
}

func TestInstallerRemoteCopyFailurePreservesOldSupportAndRetriesMigration(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	install := f.installer
	credentials := []byte(`{"enabled":true,"secret_key":"preserved fixture"}`)
	config := filepath.Join(install.config, "remote.json")
	integrationTestWrite(t, config, credentials)
	binary := filepath.Join(install.root, "bin/agent-remote")
	oldBinary := integrationTestRead(t, binary)
	outside := filepath.Join(install.home, "unrelated executable")
	integrationTestWrite(t, outside, []byte("unrelated private content"))
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, binary); err != nil {
		t.Fatal(err)
	}
	command := install.command
	migrations := 0
	install.command = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == binary {
			migrations++
		}
		return command(ctx, args)
	}
	if err := f.run(t); err == nil {
		t.Fatal("unsafe runtime destination was overwritten")
	}
	if migrations != 0 {
		t.Fatal("failed runtime copy invoked migration")
	}
	if _, err := updateInstalled(install.root, install.app); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("failed runtime copy lost pending marker: %v", err)
	}
	if string(integrationTestRead(t, outside)) != "unrelated private content" || !bytes.Equal(integrationTestRead(t, config), credentials) {
		t.Fatal("failed copy changed unrelated data or credentials")
	}
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, binary, oldBinary)
	if err := os.Chmod(binary, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if migrations != 1 {
		t.Fatalf("runtime copy retry did not reconcile old service: %d", migrations)
	}
	if _, err := updateInstalled(install.root, install.app); err != nil {
		t.Fatalf("runtime copy retry did not complete: %v", err)
	}
}

func TestInstallerUpgradePreservesPrivateDataAndExistingDaemon(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	install := f.installer
	// An embedded upgrade also retains an existing inert legacy configuration.
	integrationTestWrite(t, filepath.Join(install.config, "pueue.yml"), mustMetadataJSON(t, install.freshConfig()))
	config := integrationTestRead(t, filepath.Join(install.config, "pueue.yml"))
	privateFiles := map[string][]byte{filepath.Join(install.state, "learning.sqlite"): []byte("private database"), filepath.Join(install.state, "jobs/private-job.json"): []byte("private job"), filepath.Join(install.config, "remote.json"): []byte(`{"enabled":true,"secret_key":"fixture-secret"}`)}
	for name, data := range privateFiles {
		integrationTestWrite(t, name, data)
	}
	f.tasks = Object{"3": Object{"status": Object{"Running": Object{}}, "command": "new Go queue"}}
	f.calls = nil
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(config, integrationTestRead(t, filepath.Join(install.config, "pueue.yml"))) {
		t.Fatal("existing config changed")
	}
	for name, data := range privateFiles {
		if !bytes.Equal(integrationTestRead(t, name), data) {
			t.Fatal("private data changed")
		}
	}
	for _, args := range f.calls {
		if len(args) > 1 && args[0] == "launchctl" && (args[1] == "bootout" || (args[1] == "kickstart" && strings.HasSuffix(args[len(args)-1], "/io.went.agent-heavy"))) {
			t.Fatalf("upgrade stopped working queue: %v", args)
		}
	}
}

func TestInstallerLegacyHomebrewPythonCallbackMigratesAndRetries(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	install := f.installer
	configuration := install.freshConfig()
	configuration["daemon"].(Object)["callback"] = "/opt/homebrew/bin/python3 " + installQuote(filepath.Join(install.root, "agent_heavy.py")) + " cleanup {{ id }}"
	configuration["client"].(Object)["custom-option"] = "preserve me"
	integrationTestWrite(t, filepath.Join(install.config, "pueue.yml"), mustMetadataJSON(t, configuration))
	f.tasks = Object{"17": Object{"status": Object{"Done": Object{"result": "Success"}}, "command": "completed fixture job"}}
	private := filepath.Join(install.state, "jobs/private-job.json")
	integrationTestWrite(t, private, []byte("preserved private state"))
	if err := f.run(t); err != nil {
		t.Fatalf("standard Homebrew Python cleanup callback rejected: %v", err)
	}
	config, err := installConfig(filepath.Join(install.config, "pueue.yml"))
	if err != nil {
		t.Fatal(err)
	}
	callback, err := installConfigCallback(config)
	if err != nil || callback != installQuote(filepath.Join(install.root, "bin/agent-heavy"))+" cleanup {{ id }}" {
		t.Fatalf("cleanup callback was not migrated: %q %v", callback, err)
	}
	if installYAMLValue(installYAMLValue(config, "client"), "custom-option").Value != "preserve me" {
		t.Fatal("custom configuration lost")
	}
	snapshot, err := ReadPrivate(filepath.Join(install.state, "legacy-queue-import.json"))
	if err != nil || text(obj(obj(snapshot["tasks"])["17"])["command"]) != "completed fixture job" {
		t.Fatalf("completed queue history lost: %v %v", snapshot, err)
	}
	if err := f.run(t); err != nil {
		t.Fatalf("retry failed after callback migration: %v", err)
	}
	if string(integrationTestRead(t, private)) != "preserved private state" {
		t.Fatal("private state changed")
	}
}

func TestInstallerStandardCleanupCallbackSpellings(t *testing.T) {
	for _, interpreter := range []string{
		".venv/bin/python", ".venv/bin/python3", ".venv/bin/python3.12",
		"python3", "python3.12", "/usr/bin/python3",
		"/opt/homebrew/bin/python3", "/opt/homebrew/bin/python3.12",
		"/usr/local/bin/python3", "/usr/local/bin/python3.12",
		"/opt/homebrew/opt/python@3.12/bin/python3.12",
		"/usr/local/opt/python@3.12/bin/python3.12",
		"bin/agent-heavy",
	} {
		for _, quoting := range []string{"canonical", "single", "double", "mixed"} {
			t.Run(interpreter+"/"+quoting, func(t *testing.T) {
				f := installTestFixture(t)
				if interpreter == "bin/agent-heavy" {
					// Exercise the queue-only preflight independently of Python retirement.
					f.externalGoQueue(t)
				} else {
					f.legacy(t)
				}
				install := f.installer
				executable := interpreter
				if strings.HasPrefix(interpreter, ".venv/") || interpreter == "bin/agent-heavy" {
					executable = filepath.Join(install.root, interpreter)
				}
				script := filepath.Join(install.root, "agent_heavy.py")
				switch quoting {
				case "canonical":
					executable, script = installQuote(executable), installQuote(script)
				case "single":
					executable, script = "'"+executable+"'", "'"+script+"'"
				case "double":
					executable, script = "\""+executable+"\"", "\""+script+"\""
				case "mixed":
					executable, script = "'"+executable+"'", "\""+script+"\""
				}
				callback := executable + " " + script + " cleanup {{ id }}"
				if interpreter == "bin/agent-heavy" {
					callback = executable + " cleanup {{ id }}"
				}
				config := install.freshConfig()
				config["daemon"].(Object)["callback"] = callback
				integrationTestWrite(t, filepath.Join(install.config, "pueue.yml"), mustMetadataJSON(t, config))
				if err := f.run(t); err != nil {
					t.Fatalf("standard callback %q rejected: %v", callback, err)
				}
				if _, err := updateInstalled(install.root, install.app); err != nil {
					t.Fatalf("standard callback migration incomplete: %v", err)
				}
			})
		}
	}
}

func TestInstallerCustomCleanupCallbacksRejectBeforeTransaction(t *testing.T) {
	for _, phase := range []string{"python", "queue"} {
		for _, change := range []string{
			"semicolon", "and", "or", "pipe", "background", "newline", "redirect", "input", "comment",
			"substitution", "backticks", "wrapper", "assignment", "flag", "extra-argument", "changed-command",
			"changed-template", "quoted-template", "quoted-command", "escaped-command", "other-script", "other-interpreter", "unknown-version", "cellar-interpreter", "unterminated-quote",
		} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				f := installTestFixture(t)
				if phase == "python" {
					f.legacy(t)
				} else {
					f.externalGoQueue(t)
				}
				install := f.installer
				invocation := installQuote(filepath.Join(install.root, "bin/agent-heavy"))
				if phase == "python" {
					invocation = "/opt/homebrew/bin/python3 " + installQuote(filepath.Join(install.root, "agent_heavy.py"))
				}
				callback := invocation + " cleanup {{ id }}"
				switch change {
				case "semicolon":
					callback += "; operator-hook"
				case "and":
					callback += " && operator-hook"
				case "or":
					callback += " || operator-hook"
				case "pipe":
					callback += " | operator-hook"
				case "background":
					callback += " &"
				case "newline":
					callback += "\noperator-hook"
				case "redirect":
					callback += " >/tmp/operator-log"
				case "input":
					callback += " </tmp/operator-input"
				case "comment":
					callback += " # operator annotation"
				case "substitution":
					callback = "$(operator-hook)" + callback
				case "backticks":
					callback = "`operator-hook`" + callback
				case "wrapper":
					callback = "env " + callback
				case "assignment":
					callback = "PYTHONPATH=/operator " + callback
				case "flag":
					if phase == "python" {
						callback = strings.Replace(callback, "python3 ", "python3 -B ", 1)
					} else {
						callback = strings.Replace(callback, " cleanup", " --verbose cleanup", 1)
					}
				case "extra-argument":
					callback += " extra"
				case "changed-command":
					callback = strings.Replace(callback, " cleanup", " scheduler", 1)
				case "changed-template":
					callback = strings.Replace(callback, "{{ id }}", "{{ task_id }}", 1)
				case "quoted-template":
					callback = strings.Replace(callback, "{{ id }}", "'{{ id }}'", 1)
				case "quoted-command":
					callback = strings.Replace(callback, " cleanup", " 'cleanup'", 1)
				case "escaped-command":
					callback = strings.Replace(callback, " cleanup", ` clean\up`, 1)
				case "other-script":
					callback = "/opt/homebrew/bin/python3 " + installQuote(filepath.Join(install.root, "custom.py")) + " cleanup {{ id }}"
				case "other-interpreter":
					callback = "/operator/bin/python3 " + installQuote(filepath.Join(install.root, "agent_heavy.py")) + " cleanup {{ id }}"
				case "unknown-version":
					callback = "python3.99 " + installQuote(filepath.Join(install.root, "agent_heavy.py")) + " cleanup {{ id }}"
				case "cellar-interpreter":
					callback = "/opt/homebrew/Cellar/python@3.12/3.12.1/bin/python3.12 " + installQuote(filepath.Join(install.root, "agent_heavy.py")) + " cleanup {{ id }}"
				case "unterminated-quote":
					callback = "'" + callback
				}
				configuration := install.freshConfig()
				configuration["daemon"].(Object)["callback"] = callback
				configPath := filepath.Join(install.config, "pueue.yml")
				raw := mustMetadataJSON(t, configuration)
				integrationTestWrite(t, configPath, raw)
				private := filepath.Join(install.state, "jobs/operator-state.json")
				integrationTestWrite(t, private, []byte("operator state"))
				err := f.run(t)
				if err == nil || !strings.Contains(err.Error(), "callback is customized") {
					t.Fatalf("custom callback not rejected: %q %v", callback, err)
				}
				if !bytes.Equal(raw, integrationTestRead(t, configPath)) || string(integrationTestRead(t, private)) != "operator state" {
					t.Fatal("rejected callback changed config bytes or private state")
				}
				for _, filename := range []string{filepath.Join(install.root, "install-in-progress.json"), filepath.Join(install.state, "legacy-queue-import.json")} {
					if _, err := os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("rejected callback published transaction: %s %v", filename, err)
					}
				}
				for _, args := range f.calls {
					if len(args) < 2 || args[0] != "launchctl" || args[1] != "print" {
						t.Fatalf("rejected callback invoked a command beyond service inspection: %v", args)
					}
				}
			})
		}
	}
}

func TestInstallerCleanupCallbackLiteralInstalledPaths(t *testing.T) {
	for _, name := range []string{"plain", "literal-shell-characters", "unsafe-double-quotes"} {
		t.Run(name, func(t *testing.T) {
			f := installTestFixture(t)
			install := f.installer
			install.root = filepath.Join(filepath.Dir(install.home), "plain-root")
			if name != "plain" {
				install.root += " ' $literal `literal` \\"
			}
			f.legacy(t)
			script := installQuote(filepath.Join(install.root, "agent_heavy.py"))
			if name == "unsafe-double-quotes" {
				// Tokenization alone would hide expansions inside this quoted path.
				script = "\"" + filepath.Join(install.root, "agent_heavy.py") + "\""
			}
			config := install.freshConfig()
			config["daemon"].(Object)["callback"] = "python3 " + script + " cleanup {{ id }}"
			configPath := filepath.Join(install.config, "pueue.yml")
			raw := mustMetadataJSON(t, config)
			integrationTestWrite(t, configPath, raw)
			err := f.run(t)
			if name == "unsafe-double-quotes" {
				if err == nil || !strings.Contains(err.Error(), "callback is customized") || !bytes.Equal(raw, integrationTestRead(t, configPath)) {
					t.Fatalf("unsafe quoted callback was not preserved: %v", err)
				}
			} else if err != nil {
				t.Fatalf("literal installed path rejected: %v", err)
			}
		})
	}
}

func TestInstallerLegacyDrainedMigrationRemovesRuntimeAndUpdatesOwnedHooks(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	install := f.installer
	f.tasks = Object{"1": Object{"status": Object{"Done": Object{"result": "Success"}}, "command": "old Python command"}}
	secret := []byte(`{"enabled":false,"password_hash":"preserved fixture","secret_key":"preserved fixture"}`)
	integrationTestWrite(t, filepath.Join(install.config, "remote.json"), secret)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent_heavy.py", ".venv"} {
		if _, err := os.Stat(filepath.Join(install.root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old runtime remains %s", name)
		}
	}
	config, err := installConfig(filepath.Join(install.config, "pueue.yml"))
	if err != nil {
		t.Fatal(err)
	}
	callback, _ := installConfigCallback(config)
	if strings.Contains(callback, "python") || strings.Contains(callback, ".py") {
		t.Fatalf("old callback %s", callback)
	}
	if installYAMLValue(installYAMLValue(config, "client"), "custom-option").Value != "preserve me" {
		t.Fatal("custom configuration lost")
	}
	controller, err := updatePlist(filepath.Join(install.launch, "io.went.agent-resources.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if controller["EnvironmentVariables"].(map[string]any)["CUSTOM_OPERATOR_VALUE"] != "retained" {
		t.Fatal("operator environment lost")
	}
	if !bytes.Equal(secret, integrationTestRead(t, filepath.Join(install.config, "remote.json"))) {
		t.Fatal("remote credentials changed")
	}
	var stopped []string
	for _, args := range f.calls {
		if len(args) > 1 && args[0] == "launchctl" && args[1] == "bootout" {
			stopped = append(stopped, filepath.Base(args[2]))
		}
	}
	if strings.Join(stopped, ",") != "io.went.agent-resources,io.went.agent-heavy" {
		t.Fatalf("legacy services not retired %v", stopped)
	}
}

func TestInstallerLegacyExplicitProgramMigratesQueueAndController(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	install := f.installer
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources"} {
		filename := filepath.Join(install.launch, label+".plist")
		definition, err := updatePlist(filename)
		if err != nil {
			t.Fatal(err)
		}
		args, err := stringsFrom(definition["ProgramArguments"])
		if err != nil {
			t.Fatal(err)
		}
		definition["Program"] = args[0]
		definition["ThrottleInterval"] = 37
		if err := install.writePlist(filename, definition); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	for label, command := range map[string]string{"io.went.agent-heavy": "daemon", "io.went.agent-resources": "scheduler"} {
		definition, err := updatePlist(filepath.Join(install.launch, label+".plist"))
		if err != nil {
			t.Fatal(err)
		}
		if _, present := definition["Program"]; present {
			t.Fatalf("%s retained obsolete explicit Program", label)
		}
		args, err := stringsFrom(definition["ProgramArguments"])
		if err != nil || !slices.Equal(args, []string{filepath.Join(install.root, "bin/agent-heavy"), command}) {
			t.Fatalf("%s did not adopt the installed Go executable: %v %v", label, args, err)
		}
		if interval, valid := integer(definition["ThrottleInterval"]); !valid || interval != 37 {
			t.Fatalf("%s lost its operator setting", label)
		}
	}
}

func TestInstallerLegacyMismatchedProgramRejectsBeforeShutdown(t *testing.T) {
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources"} {
		t.Run(label, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			install := f.installer
			source := filepath.Join(install.root, "agent_heavy.py")
			original := integrationTestRead(t, source)
			filename := filepath.Join(install.launch, label+".plist")
			definition, err := updatePlist(filename)
			if err != nil {
				t.Fatal(err)
			}
			definition["Program"] = filepath.Join(install.home, "different-runtime")
			if err := install.writePlist(filename, definition); err != nil {
				t.Fatal(err)
			}
			before := integrationTestRead(t, filename)
			if err := f.run(t); err == nil || !strings.Contains(err.Error(), "Program differs") {
				t.Fatalf("mismatched explicit executable accepted: %v", err)
			}
			if !bytes.Equal(original, integrationTestRead(t, source)) || !bytes.Equal(before, integrationTestRead(t, filename)) {
				t.Fatal("Program preflight changed existing source or service")
			}
			for _, args := range f.calls {
				if len(args) > 1 && args[0] == "launchctl" && (args[1] == "bootout" || args[1] == "bootstrap" || args[1] == "kickstart") {
					t.Fatal("Program preflight modified a running service")
				}
			}
			for _, path := range []string{filepath.Join(install.root, "install-in-progress.json"), filepath.Join(install.state, "legacy-queue-import.json")} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("Program preflight published migration state")
				}
			}
		})
	}
}

func TestInstallerReservedServiceEnvironmentMismatchRejectsBeforeMutation(t *testing.T) {
	for _, phase := range []string{"legacy", "embedded"} {
		for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources"} {
			for _, key := range []string{"HOME", "AGENT_HEAVY_STATE", "AGENT_HEAVY_CONFIG"} {
				t.Run(phase+"/"+label+"/"+key, func(t *testing.T) {
					f := installTestFixture(t)
					if phase == "legacy" {
						f.legacy(t)
					} else if err := f.run(t); err != nil {
						t.Fatal(err)
					}
					install := f.installer
					filename := filepath.Join(install.launch, label+".plist")
					definition, err := updatePlist(filename)
					if err != nil {
						t.Fatal(err)
					}
					environment := obj(definition["EnvironmentVariables"])
					if environment == nil {
						environment = Object{}
					}
					environment[key] = filepath.Join(install.home, "different-target")
					definition["EnvironmentVariables"] = environment
					if err := install.writePlist(filename, definition); err != nil {
						t.Fatal(err)
					}
					source := filepath.Join(install.root, "bin/agent-heavy")
					if phase == "legacy" {
						source = filepath.Join(install.root, "agent_heavy.py")
					}
					original := integrationTestRead(t, source)
					before := integrationTestRead(t, filename)
					importPath := filepath.Join(install.state, "legacy-queue-import.json")
					importMarker := []byte(`{"tasks":{},"preserve":"incomplete import"}`)
					integrationTestWrite(t, importPath, importMarker)
					f.calls = nil
					if err := f.run(t); err == nil || !strings.Contains(err.Error(), key+" differs") {
						t.Fatalf("reserved environment mismatch accepted: %v", err)
					}
					if !bytes.Equal(original, integrationTestRead(t, source)) || !bytes.Equal(before, integrationTestRead(t, filename)) || !bytes.Equal(importMarker, integrationTestRead(t, importPath)) {
						t.Fatal("environment preflight changed source, service, or import marker")
					}
					for _, args := range f.calls {
						if len(args) > 1 && args[0] == "launchctl" && (args[1] == "bootout" || args[1] == "bootstrap" || args[1] == "kickstart") {
							t.Fatal("environment preflight changed a running service")
						}
					}
					if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("environment preflight published an installation marker")
					}
				})
			}
		}
	}
}

func TestInstallerConfiguredServiceSlotRemainsAuthoritative(t *testing.T) {
	for _, phase := range []string{"legacy", "embedded"} {
		t.Run(phase, func(t *testing.T) {
			f := installTestFixture(t)
			if phase == "legacy" {
				f.legacy(t)
			} else if err := f.run(t); err != nil {
				t.Fatal(err)
			}
			install := f.installer
			slot := filepath.Join(install.home, "Configured Queue Slot")
			expected := Object{"HOME": install.home, "AGENT_HEAVY_STATE": install.state, "AGENT_HEAVY_CONFIG": filepath.Join(install.config, "pueue.yml"), "AGENT_HEAVY_LEGACY_SLOT": slot, "CUSTOM_OPERATOR_VALUE": "retained"}
			for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources"} {
				filename := filepath.Join(install.launch, label+".plist")
				definition, err := updatePlist(filename)
				if err != nil {
					t.Fatal(err)
				}
				definition["EnvironmentVariables"] = expected
				if err := install.writePlist(filename, definition); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("AGENT_HEAVY_LEGACY_SLOT", filepath.Join(t.TempDir(), "unrelated-ambient-slot"))
			if err := f.run(t); err != nil {
				t.Fatal(err)
			}
			if install.serviceSlot != slot {
				t.Fatal("validated service slot was not retained")
			}
			for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources"} {
				definition, err := updatePlist(filepath.Join(install.launch, label+".plist"))
				if err != nil {
					t.Fatal(err)
				}
				actual := obj(definition["EnvironmentVariables"])
				for key, value := range expected {
					if actual[key] != value {
						t.Fatalf("configured service environment changed %s", key)
					}
				}
			}
			foundSlot := false
			for _, entry := range install.commandEnvironment([]string{filepath.Join(install.root, "bin/agent-heavy"), "status", "--json"}) {
				if strings.HasPrefix(entry, "AGENT_HEAVY_LEGACY_SLOT=") {
					foundSlot = true
					if entry != "AGENT_HEAVY_LEGACY_SLOT="+slot {
						t.Fatal("health probe did not honor the configured service slot")
					}
				}
			}
			if !foundSlot {
				t.Fatal("health probe omitted the configured service slot")
			}
			integrationTestWrite(t, filepath.Join(slot, "agent-heavy.json"), []byte(`{"task_id":7,"pgid":123456}`))
			if err := install.requireLegacyCleanup(); !errors.Is(err, errInstallPendingCleanup) {
				t.Fatalf("cleanup inspection ignored configured service slot: %v", err)
			}
		})
	}
}

func TestInstallerConflictingServiceSlotsRejectBeforeShutdown(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	filename := filepath.Join(f.installer.launch, "io.went.agent-resources.plist")
	definition, err := updatePlist(filename)
	if err != nil {
		t.Fatal(err)
	}
	obj(definition["EnvironmentVariables"])["AGENT_HEAVY_LEGACY_SLOT"] = filepath.Join(f.installer.home, "Different Slot")
	if err := f.installer.writePlist(filename, definition); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t); err == nil || !strings.Contains(err.Error(), "different legacy slots") {
		t.Fatalf("conflicting service slots accepted: %v", err)
	}
	for _, args := range f.calls {
		if len(args) > 1 && args[0] == "launchctl" && args[1] == "bootout" {
			t.Fatal("conflicting service slots stopped a legacy service")
		}
	}
}

func (f *installFixture) externalGoQueue(t *testing.T) {
	t.Helper()
	f.legacy(t)
	install := f.installer
	for _, name := range []string{"agent_heavy.py", ".venv"} {
		if err := os.RemoveAll(filepath.Join(install.root, name)); err != nil {
			t.Fatal(err)
		}
	}
	integrationTestWrite(t, filepath.Join(install.config, "pueue.yml"), mustMetadataJSON(t, install.freshConfig()))
	if err := install.writePlist(filepath.Join(install.launch, "io.went.agent-resources.plist"), Object{"Label": "io.went.agent-resources", "ProgramArguments": []string{filepath.Join(install.root, "bin/agent-heavy"), "scheduler"}}); err != nil {
		t.Fatal(err)
	}
}

func TestInstallerExternalGoQueueRequiresCompleteDrain(t *testing.T) {
	for _, status := range []string{"Running", "Paused", "Queued", "Stashed", "Locked"} {
		t.Run(status, func(t *testing.T) {
			f := installTestFixture(t)
			f.externalGoQueue(t)
			f.tasks = Object{"41": Object{"status": Object{status: Object{}}}}
			if err := f.run(t); err == nil || !strings.Contains(err.Error(), "drained") {
				t.Fatalf("undrained external Go queue accepted: %v", err)
			}
			if !f.loaded["io.went.agent-heavy"] || !f.loaded["io.went.agent-resources"] {
				t.Fatal("preflight stopped existing work")
			}
			if _, err := os.Stat(filepath.Join(f.installer.root, "install-in-progress.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("undrained migration published installation")
			}
		})
	}
}

func TestInstallerExternalGoDrainedSnapshotAndSettingsPreserved(t *testing.T) {
	f := installTestFixture(t)
	f.externalGoQueue(t)
	install := f.installer
	f.tasks = Object{"401": Object{"id": 401, "status": Object{"Done": Object{"result": "Success"}}, "command": "retained historical command"}}
	f.groups = Object{"default": Object{"parallel_tasks": 3, "status": "Paused"}}
	config := integrationTestRead(t, filepath.Join(install.config, "pueue.yml"))
	private := map[string][]byte{
		filepath.Join(install.state, "metadata/401.json"):                           []byte(`{"id":401,"token":"retained"}`),
		filepath.Join(install.state, "pueue/task_logs/401.log"):                     []byte("retained private output"),
		filepath.Join(install.state, "learning.sqlite"):                             []byte("retained resource history"),
		filepath.Join(install.config, "remote.json"):                                []byte(`{"enabled":true,"password_hash":"retained hash","secret_key":"retained secret"}`),
		filepath.Join(install.state, "remote/caddy/pki/authorities/local/root.crt"): []byte("retained private CA"),
	}
	for name, data := range private {
		integrationTestWrite(t, name, data)
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadPrivate(filepath.Join(install.state, "legacy-queue-import.json"))
	if err != nil || text(obj(obj(snapshot["tasks"])["401"])["command"]) != "retained historical command" {
		t.Fatalf("completed job history was not retained: %v %v", snapshot, err)
	}
	if limit, status, err := legacyQueueSettings(snapshot); err != nil || limit != 3 || status != "Paused" {
		t.Fatalf("live queue settings were not retained after shutdown: %v %s %v", limit, status, err)
	}
	if !bytes.Equal(config, integrationTestRead(t, filepath.Join(install.config, "pueue.yml"))) {
		t.Fatal("existing queue settings changed")
	}
	for name, expected := range private {
		if !bytes.Equal(expected, integrationTestRead(t, name)) {
			t.Fatalf("private state changed: %s", name)
		}
	}
	definition, err := updatePlist(filepath.Join(install.launch, "io.went.agent-heavy.plist"))
	if err != nil {
		t.Fatal(err)
	}
	args, err := stringsFrom(definition["ProgramArguments"])
	if err != nil || !slices.Equal(args, []string{filepath.Join(install.root, "bin/agent-heavy"), "daemon"}) {
		t.Fatalf("legacy queue service not replaced: %v %v", args, err)
	}
}

func TestInstallerOfflineCompressedLegacyStateNeedsNoPueue(t *testing.T) {
	f := installTestFixture(t)
	f.externalGoQueue(t)
	f.loaded = map[string]bool{}
	install := f.installer
	for _, name := range []string{"brew", "pueue", "pueued"} {
		if err := os.Remove(filepath.Join(install.path, name)); err != nil {
			t.Fatal(err)
		}
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	snapshot := Object{"tasks": Object{"52": Object{"id": 52, "status": Object{"Done": Object{"result": "Success"}}}}, "groups": Object{"default": Object{"parallel_tasks": 3, "status": "Paused"}}}
	if _, err := writer.Write(mustMetadataJSON(t, snapshot)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(install.state, "pueue/state.json.gz"), compressed.Bytes())
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	imported, err := ReadPrivate(filepath.Join(install.state, "legacy-queue-import.json"))
	if err != nil || text(obj(obj(imported["groups"])["default"])["status"]) != "Paused" || obj(imported["tasks"])["52"] == nil {
		t.Fatalf("compressed queue settings/history lost: %v %v", imported, err)
	}
}

func TestInstallerMalformedLegacyGroupSettingsRejectBeforeShutdown(t *testing.T) {
	for _, transport := range []string{"live", "offline"} {
		for _, malformed := range []string{"missing-groups", "missing-default", "missing-limit", "missing-status", "invalid-limit", "invalid-status"} {
			t.Run(transport+"/"+malformed, func(t *testing.T) {
				f := installTestFixture(t)
				f.legacy(t)
				install := f.installer
				snapshot := Object{"tasks": Object{}}
				group := Object{"parallel_tasks": 2, "status": "Paused"}
				switch malformed {
				case "missing-groups":
				case "missing-default":
					snapshot["groups"] = Object{}
				case "missing-limit":
					delete(group, "parallel_tasks")
					snapshot["groups"] = Object{"default": group}
				case "missing-status":
					delete(group, "status")
					snapshot["groups"] = Object{"default": group}
				case "invalid-limit":
					group["parallel_tasks"] = 0
					snapshot["groups"] = Object{"default": group}
				case "invalid-status":
					group["status"] = "Unknown"
					snapshot["groups"] = Object{"default": group}
				}
				encoded := mustMetadataJSON(t, snapshot)
				if transport == "offline" {
					f.loaded = map[string]bool{}
					integrationTestWrite(t, filepath.Join(install.state, "pueue/state.json"), encoded)
				} else {
					command := install.command
					install.command = func(ctx context.Context, args []string) ([]byte, error) {
						if args[0] == "pueue" && strings.Contains(strings.Join(args, " "), "status --json") {
							return encoded, nil
						}
						return command(ctx, args)
					}
				}
				source := integrationTestRead(t, filepath.Join(install.root, "agent_heavy.py"))
				configuration := integrationTestRead(t, filepath.Join(install.config, "pueue.yml"))
				if err := f.run(t); err == nil || !strings.Contains(err.Error(), "legacy queue settings") {
					t.Fatalf("malformed settings accepted: %v", err)
				}
				if !bytes.Equal(source, integrationTestRead(t, filepath.Join(install.root, "agent_heavy.py"))) || !bytes.Equal(configuration, integrationTestRead(t, filepath.Join(install.config, "pueue.yml"))) {
					t.Fatal("malformed settings changed legacy source/configuration")
				}
				for _, args := range f.calls {
					if len(args) > 1 && args[0] == "launchctl" && args[1] == "bootout" {
						t.Fatal("malformed settings stopped an existing service")
					}
				}
				for _, filename := range []string{filepath.Join(install.root, "install-in-progress.json"), filepath.Join(install.state, "legacy-queue-import.json")} {
					if _, err := os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("malformed settings published migration state")
					}
				}
			})
		}
	}
}

func TestInstallerAbsentLegacyStatePublishesExplicitFreshDefaults(t *testing.T) {
	f := installTestFixture(t)
	f.externalGoQueue(t)
	f.loaded = map[string]bool{}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadPrivate(filepath.Join(f.installer.state, "legacy-queue-import.json"))
	if err != nil {
		t.Fatal(err)
	}
	if limit, status, err := legacyQueueSettings(snapshot); err != nil || limit != 1 || status != "Running" || len(obj(snapshot["tasks"])) != 0 {
		t.Fatalf("absent legacy state lacks explicit fresh defaults: %v %s %v", limit, status, err)
	}
}

func TestInstallerLegacyLoadedOwnershipMismatchRefusesShutdown(t *testing.T) {
	f := installTestFixture(t)
	f.externalGoQueue(t)
	command := f.installer.command
	f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[0] == "launchctl" && args[1] == "print" && strings.HasSuffix(args[2], "/io.went.agent-heavy") {
			return []byte("program = /different/daemon\narguments = {\n/different/daemon\n}\n"), nil
		}
		return command(ctx, args)
	}
	if err := f.run(t); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("unowned loaded daemon accepted: %v", err)
	}
	if !f.loaded["io.went.agent-heavy"] {
		t.Fatal("unowned daemon was stopped")
	}
}

func TestInstallerOwnershipParsesRealPrivateLaunchctlService(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS launchctl ownership integration")
	}
	label := "io.went.agent-queue-installer-test-" + token()
	domain := "gui/" + strconv.Itoa(os.Getuid())
	args := []string{"/usr/bin/true", "--config", filepath.Join(t.TempDir(), "Path With Spaces", "pueue.yml")}
	filename := filepath.Join(t.TempDir(), "private-service.plist")
	raw, err := plist.Marshal(Object{"Label": label, "ProgramArguments": args, "RunAtLoad": false}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/bin/launchctl", "bootstrap", domain, filename).CombinedOutput(); err != nil {
		t.Skipf("private GUI launch agent unavailable: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("/bin/launchctl", "bootout", domain+"/"+label).CombinedOutput(); err != nil {
			t.Errorf("private launch agent cleanup: %v: %s", err, output)
		}
	})
	output, err := exec.Command("/bin/launchctl", "print", domain+"/"+label).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !installLoadedServiceMatches(output, args) {
		t.Fatal("real launchctl executable/arguments were not recognized")
	}
	wrong := slices.Clone(args)
	wrong[2] += ".different"
	if installLoadedServiceMatches(output, wrong) {
		t.Fatal("different private configuration was accepted")
	}
	if installLoadedServiceMatches(append(output, []byte("\nprogram = /different/program\n")...), args) {
		t.Fatal("ambiguous loaded executable was accepted")
	}
}

func TestInstallerLegacyStateRecheckedAfterDaemonStops(t *testing.T) {
	f := installTestFixture(t)
	f.externalGoQueue(t)
	command := f.installer.command
	f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[0] == "launchctl" && args[1] == "bootout" && strings.HasSuffix(args[2], "/io.went.agent-heavy") {
			f.tasks = Object{"42": Object{"status": Object{"Queued": Object{}}}}
		}
		return command(ctx, args)
	}
	if err := f.run(t); err == nil || !strings.Contains(err.Error(), "drained") {
		t.Fatalf("newly persisted waiting task accepted: %v", err)
	}
	if f.installer.embeddedDefinition() {
		t.Fatal("unsafe post-stop queue replaced its service definition")
	}
	if _, err := os.Stat(filepath.Join(f.installer.state, "legacy-queue-import.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsafe post-stop queue published an import")
	}
}

func TestInstallerLegacyDoneJobsRequireCompletedCleanup(t *testing.T) {
	for _, kind := range []string{"unstarted-receipt", "pgid-receipt-and-marker", "orphan-marker", "shared-marker"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			install := f.installer
			f.tasks = Object{"41": Object{"id": 41, "status": Object{"Done": Object{"result": "Success"}}}}
			source := integrationTestRead(t, filepath.Join(install.root, "agent_heavy.py"))
			marker := Object{"task_id": 41, "token": "retained-token", "pgid": 123456}
			var filename string
			switch kind {
			case "unstarted-receipt":
				filename = filepath.Join(install.state, "receipts/41.json")
				integrationTestWrite(t, filename, []byte(`{"task_id":41,"token":"unstarted"}`))
			case "pgid-receipt-and-marker":
				integrationTestWrite(t, filepath.Join(install.state, "receipts/41.json"), mustMetadataJSON(t, marker))
				filename = filepath.Join(install.state, "execution.slot/agent-heavy.json")
				integrationTestWrite(t, filename, mustMetadataJSON(t, marker))
			case "orphan-marker":
				filename = filepath.Join(install.state, "execution.slot/agent-heavy.json")
				integrationTestWrite(t, filename, mustMetadataJSON(t, marker))
			case "shared-marker":
				filename = filepath.Join(install.state, "execution.slot/agent-heavy-jobs/41-retained-token.json")
				integrationTestWrite(t, filename, mustMetadataJSON(t, marker))
			}
			retained := integrationTestRead(t, filename)
			if err := f.run(t); !errors.Is(err, errInstallPendingCleanup) {
				t.Fatalf("unfinished cleanup accepted: %v", err)
			}
			if !bytes.Equal(source, integrationTestRead(t, filepath.Join(install.root, "agent_heavy.py"))) || !bytes.Equal(retained, integrationTestRead(t, filename)) {
				t.Fatal("cleanup preflight changed existing source or ownership records")
			}
			for _, args := range f.calls {
				if len(args) > 1 && args[0] == "launchctl" && args[1] == "bootout" {
					t.Fatal("unfinished cleanup preflight stopped a legacy service")
				}
			}
			if _, err := os.Stat(filepath.Join(install.state, "legacy-queue-import.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unfinished cleanup published an import")
			}
		})
	}
}

func TestInstallerLegacyLateReceiptPreventsPublication(t *testing.T) {
	f := installTestFixture(t)
	f.externalGoQueue(t)
	install := f.installer
	old := []byte("retained old Go executable")
	integrationTestWrite(t, filepath.Join(install.root, "bin/agent-heavy"), old)
	command := install.command
	install.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[0] == "launchctl" && args[1] == "bootout" && strings.HasSuffix(args[2], "/io.went.agent-heavy") {
			integrationTestWrite(t, filepath.Join(install.state, "receipts/42.json"), []byte(`{"task_id":42,"token":"late"}`))
		}
		return command(ctx, args)
	}
	if err := f.run(t); !errors.Is(err, errInstallPendingCleanup) {
		t.Fatalf("late receipt was accepted: %v", err)
	}
	if !bytes.Equal(old, integrationTestRead(t, filepath.Join(install.root, "bin/agent-heavy"))) {
		t.Fatal("late receipt replaced old executable")
	}
	if _, err := os.Stat(filepath.Join(install.state, "legacy-queue-import.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("late receipt published an import")
	}
}

func TestInstallerLegacyUnrelatedExternalSlotMayRemainOccupied(t *testing.T) {
	f := installTestFixture(t)
	f.externalGoQueue(t)
	owner := filepath.Join(f.installer.state, "execution.slot/owner")
	integrationTestWrite(t, owner, []byte("unrelated external validator"))
	if err := f.run(t); err != nil {
		t.Fatalf("unrelated external legacy occupancy blocked migration: %v", err)
	}
	if string(integrationTestRead(t, owner)) != "unrelated external validator" {
		t.Fatal("migration changed unrelated external ownership")
	}
}

func TestInstallerLegacyRetryAfterEmbeddedPublication(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	config := f.installer.freshConfig()
	config["daemon"].(Object)["callback"] = `"/opt/homebrew/opt/python@3.12/bin/python3.12" "` + filepath.Join(f.installer.root, "agent_heavy.py") + `" cleanup {{ id }}`
	integrationTestWrite(t, filepath.Join(f.installer.config, "pueue.yml"), mustMetadataJSON(t, config))
	f.fail = "bootstrap "
	if err := f.run(t); err == nil {
		t.Fatal("fixture publication failure accepted")
	}
	if !f.installer.embeddedDefinition() {
		t.Fatal("failure happened before embedded service publication")
	}
	f.fail = ""
	if err := f.run(t); err != nil {
		t.Fatalf("retry failed after embedded publication: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.installer.root, "agent_heavy.py")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retry did not retire old Python source")
	}
}

func TestInstallerLegacyActiveQueuesAndRemoteFailBeforeReplacement(t *testing.T) {
	for _, kind := range []string{"Running", "Paused", "Queued", "Stashed", "remote-enabled", "remote-loaded", "unknown-source", "unknown-callback", "unowned-process"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			install := f.installer
			source := filepath.Join(install.root, "agent_heavy.py")
			original := integrationTestRead(t, source)
			configuration := integrationTestRead(t, filepath.Join(install.config, "pueue.yml"))
			switch kind {
			case "remote-enabled":
				integrationTestWrite(t, filepath.Join(install.config, "remote.json"), []byte(`{"enabled":true}`))
			case "remote-loaded":
				f.loaded["io.went.agent-remote"] = true
			case "unknown-source":
				original = []byte("class Gate: pass\n")
				integrationTestWrite(t, source, original)
			case "unknown-callback":
				config := install.freshConfig()
				config["daemon"].(Object)["callback"] = "python custom-hook.py {{ id }}"
				configuration = mustMetadataJSON(t, config)
				integrationTestWrite(t, filepath.Join(install.config, "pueue.yml"), configuration)
			case "unowned-process":
				command := install.command
				install.command = func(ctx context.Context, args []string) ([]byte, error) {
					if args[0] == "ps" {
						return []byte("123 " + filepath.Join(install.root, ".venv/bin/python") + " unexplained.py"), nil
					}
					return command(ctx, args)
				}
			default:
				f.tasks = Object{"4": Object{"status": Object{kind: Object{}}}}
			}
			if err := f.run(t); err == nil {
				t.Fatal("unsafe Python migration accepted")
			}
			if !bytes.Equal(original, integrationTestRead(t, source)) || !bytes.Equal(configuration, integrationTestRead(t, filepath.Join(install.config, "pueue.yml"))) {
				t.Fatal("failed preflight changed installed support/config")
			}
			if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed preflight published transaction")
			}
		})
	}
}

func TestInstallerLegacyPhoneAccessRecovery(t *testing.T) {
	for _, kind := range []string{"enabled", "loaded", "disable-failed", "still-enabled", "undrained", "unsafe-source"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			install := f.installer
			config := filepath.Join(install.config, "remote.json")
			credentials := Object{"enabled": kind != "loaded", "password_hash": "retained hash", "secret_key": "retained secret", "port": 8743}
			integrationTestWrite(t, config, mustMetadataJSON(t, credentials))
			certificate := filepath.Join(install.state, "remote/caddy/pki/authorities/local/root.crt")
			integrationTestWrite(t, certificate, []byte("retained CA"))
			f.loaded["io.went.agent-remote"] = kind == "loaded"
			if kind == "loaded" {
				definition := Object{"Label": "io.went.agent-remote", "ProgramArguments": []string{filepath.Join(install.root, ".venv/bin/python"), filepath.Join(install.root, "remote/control.py"), "serve"}}
				raw, err := plist.Marshal(definition, plist.XMLFormat)
				if err != nil {
					t.Fatal(err)
				}
				integrationTestWrite(t, filepath.Join(install.launch, "io.went.agent-remote.plist"), raw)
			}
			if err := f.run(t); !errors.Is(err, errInstallLegacyPhoneAccess) {
				t.Fatalf("legacy blocker is not recoverable: %v", err)
			}
			install.disablePhoneAccess = true
			if kind == "undrained" {
				f.tasks = Object{"1": Object{"status": Object{"Running": Object{}}}}
			}
			if kind == "unsafe-source" {
				if err := os.Chmod(filepath.Join(install.root, "agent_heavy.py"), 0666); err != nil {
					t.Fatal(err)
				}
			}
			command := install.command
			disables := 0
			install.command = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == filepath.Join(install.payload, "source/bin/agent-remote") {
					if !slices.Equal(args[1:], []string{"disable"}) {
						t.Fatalf("unexpected recovery command: %v", args)
					}
					disables++
					if err := installDirectory(filepath.Join(install.state, "remote")); err != nil {
						t.Fatal(err)
					}
					lock, err := installLock(filepath.Join(install.state, "remote"), "control.lock")
					if err != nil {
						t.Fatalf("recovery would deadlock the remote controller: %v", err)
					}
					lock.Close()
					if kind == "disable-failed" {
						return nil, errors.New("fixture remote ownership failure")
					}
					if kind != "still-enabled" {
						credentials["enabled"] = false
						integrationTestWrite(t, config, mustMetadataJSON(t, credentials))
						delete(f.loaded, "io.went.agent-remote")
					}
					return nil, nil
				}
				return command(ctx, args)
			}
			err := f.run(t)
			if kind == "enabled" || kind == "loaded" {
				if err != nil || disables != 1 {
					t.Fatalf("explicit recovery did not finish: disables=%d, error=%v", disables, err)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe or incomplete recovery was accepted")
				}
				if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed recovery published an installation")
				}
				if kind == "unsafe-source" && disables != 0 {
					t.Fatal("unsafe legacy support permitted recovery execution")
				}
			}
			settings, err := ReadPrivate(config)
			port, validPort := integer(settings["port"])
			if err != nil || settings["password_hash"] != "retained hash" || settings["secret_key"] != "retained secret" || !validPort || port != 8743 || string(integrationTestRead(t, certificate)) != "retained CA" {
				t.Fatalf("recovery changed credentials, configuration or CA: %v", err)
			}
		})
	}
}

func TestInstallerPhoneAccessRecoveryRejectsUnownedServices(t *testing.T) {
	for _, kind := range []string{"missing", "unsafe", "symlink", "arguments", "label", "program", "loaded-mismatch", "query-failed", "home", "test-mode"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			install := f.installer
			install.disablePhoneAccess = true
			f.loaded["io.went.agent-remote"] = true
			config := filepath.Join(install.config, "remote.json")
			original := []byte(`{"enabled":true,"password_hash":"retained hash","secret_key":"retained secret"}`)
			integrationTestWrite(t, config, original)
			filename := filepath.Join(install.launch, "io.went.agent-remote.plist")
			definition := Object{"Label": "io.went.agent-remote", "ProgramArguments": []string{filepath.Join(install.root, ".venv/bin/python"), filepath.Join(install.root, "remote/control.py"), "serve"}}
			switch kind {
			case "arguments":
				definition["ProgramArguments"] = []string{"/usr/bin/unrelated", "serve"}
			case "label":
				definition["Label"] = "unrelated"
			case "program":
				definition["Program"] = "/usr/bin/unrelated"
			case "home":
				definition["EnvironmentVariables"] = Object{"HOME": filepath.Join(install.home, "unrelated")}
			case "test-mode":
				definition["EnvironmentVariables"] = Object{"AGENT_REMOTE_TEST_MODE": "1"}
			}
			if kind != "missing" {
				raw, err := plist.Marshal(definition, plist.XMLFormat)
				if err != nil {
					t.Fatal(err)
				}
				integrationTestWrite(t, filename, raw)
			}
			if kind == "unsafe" {
				if err := os.Chmod(filename, 0666); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err := os.Rename(filename, filename+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filename+".target", filename); err != nil {
					t.Fatal(err)
				}
			}
			command := install.command
			install.command = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == filepath.Join(install.payload, "source/bin/agent-remote") || (args[0] == "launchctl" && args[1] == "bootout") {
					t.Fatalf("unowned service permitted a shutdown command: %v", args)
				}
				if kind == "loaded-mismatch" && args[0] == "launchctl" && args[1] == "print" && filepath.Base(args[2]) == "io.went.agent-remote" {
					return []byte("program = /usr/bin/unrelated\narguments = {\n/usr/bin/unrelated\nserve\n}\n"), nil
				}
				if kind == "query-failed" && args[0] == "launchctl" && args[1] == "print" && filepath.Base(args[2]) == "io.went.agent-remote" {
					return nil, errors.New("fixture service inspection failed")
				}
				return command(ctx, args)
			}
			if err := f.run(t); err == nil {
				t.Fatal("unowned phone service accepted")
			}
			if !bytes.Equal(original, integrationTestRead(t, config)) {
				t.Fatal("unowned service changed remote settings")
			}
		})
	}
}

func TestInstallMainLegacyPhoneAccessRecoveryDiagnostic(t *testing.T) {
	f := installTestFixture(t)
	f.legacy(t)
	launchctl := filepath.Join(f.installer.path, "launchctl")
	integrationTestWrite(t, launchctl, []byte(fmt.Sprintf("#!/bin/sh\nprintf 'Bad request.\\nCould not find service \"%%s\" in domain for user gui: %d\\n' \"${2##*/}\" >&2\nexit 113\n", os.Getuid())))
	if err := os.Chmod(launchctl, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_MODE", "1")
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_TOOLS", f.installer.path)
	integrationTestWrite(t, filepath.Join(f.installer.config, "remote.json"), []byte(`{"enabled":true}`))
	for _, recovery := range []bool{false, true} {
		args := []string{"--payload", f.installer.payload}
		if recovery {
			args = append(args, "--disable-phone-access")
			binary := filepath.Join(f.installer.payload, "source/bin/agent-remote")
			integrationTestWrite(t, binary, []byte("#!/bin/sh\nexit 99\n"))
			if err := os.Chmod(binary, 0700); err != nil {
				t.Fatal(err)
			}
		}
		output, err := os.CreateTemp(t.TempDir(), "installer-stderr")
		if err != nil {
			t.Fatal(err)
		}
		original := os.Stderr
		os.Stderr = output
		code := InstallMain(args)
		os.Stderr = original
		if err := output.Close(); err != nil {
			t.Fatal(err)
		}
		message, err := os.ReadFile(output.Name())
		if err != nil {
			t.Fatal(err)
		}
		marker := strings.Contains(string(message), "\n"+installDisablePhoneAccessRecovery+"\n")
		if code != 1 || marker == recovery {
			t.Fatalf("recovery=%v: exit=%d marker=%v diagnostic=%s", recovery, code, marker, message)
		}
		if recovery && !strings.Contains(string(message), "Phone access could not be disabled") {
			t.Fatalf("recovery failed without actionable diagnostic: %s", message)
		}
	}
}

func TestInstallerRemoteServiceAbsenceRequiresExactEvidence(t *testing.T) {
	missing := fmt.Sprintf("Bad request.\nCould not find service \"io.went.agent-remote\" in domain for user gui: %d\n", os.Getuid())
	for _, test := range []struct {
		output string
		code   int
		absent bool
	}{
		{missing, 113, true},
		{missing, 1, false},
		{missing, 0, false},
		{"inspection timed out", 113, false},
		{strings.ReplaceAll(missing, "io.went.agent-remote", "unrelated"), 113, false},
		{missing + "unexpected diagnostic", 113, false},
	} {
		if got := installRemoteServiceAbsent([]byte(test.output), test.code); got != test.absent {
			t.Fatalf("exit %d diagnostic %q: absence=%v", test.code, test.output, got)
		}
	}
}

func TestInstallMainPhoneAccessRecoverySyntax(t *testing.T) {
	for _, args := range [][]string{
		{"--payload", "/payload", "--disable-phone-acces"},
		{"--disable-phone-access", "--payload", "/payload"},
		{"--payload", "/payload", "--disable-phone-access", "--disable-phone-access"},
		{"--app-bundle", "relative.app", "--disable-phone-access"},
	} {
		if code := InstallMain(args); code != 2 {
			t.Fatalf("invalid recovery invocation %v returned %d", args, code)
		}
	}
}

func TestInstallerPhoneAccessRecoveryEnvironment(t *testing.T) {
	f := installTestFixture(t)
	for _, key := range []string{"AGENT_REMOTE_TEST_MODE", "AGENT_REMOTE_STATE", "AGENT_REMOTE_CONFIG", "AGENT_HEAVY_STATE", "PUEUE_CONFIG_PATH"} {
		t.Setenv(key, "unrelated-fixture")
	}
	t.Setenv("HOME", filepath.Join(t.TempDir(), "unrelated-home"))
	environment := f.installer.commandEnvironment([]string{filepath.Join(f.installer.payload, "source/bin/agent-remote"), "disable"})
	if !slices.Contains(environment, "HOME="+f.installer.home) {
		t.Fatal("recovery escaped the installed user home")
	}
	for _, entry := range environment {
		if strings.HasPrefix(entry, "AGENT_REMOTE_") || strings.HasPrefix(entry, "AGENT_HEAVY_") || strings.HasPrefix(entry, "PUEUE_") {
			t.Fatalf("recovery inherited an unrelated override: %s", strings.SplitN(entry, "=", 2)[0])
		}
	}
}

func TestInstallerPhoneAccessRecoveryDoesNotDisableGoService(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	f.installer.disablePhoneAccess = true
	settings := []byte(`{"enabled":true,"password_hash":"retained hash","secret_key":"retained secret"}`)
	config := filepath.Join(f.installer.config, "remote.json")
	integrationTestWrite(t, config, settings)
	f.calls = nil
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	for _, args := range f.calls {
		if len(args) > 1 && args[1] == "disable" {
			t.Fatalf("legacy recovery disabled a Go service: %v", args)
		}
	}
	if !bytes.Equal(integrationTestRead(t, config), settings) {
		t.Fatal("legacy recovery changed modern Go access")
	}
}

func TestInstallerLegacyLeaseParsingNeverExecutesSource(t *testing.T) {
	for _, expression := range []string{`lease_slot(self.state)`, `Path(os.environ.get(OVERRIDES[2], self.state / "execution.slot"))`, `Path(os.environ.get("AGENT_HEAVY_LEGACY_SLOT", '/private/fixture/older queue.slot'))`} {
		t.Run(expression, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			source := "raise RuntimeError('must not execute')\nclass Gate:\n    def __init__(self):\n        self.slot = " + expression + "\n"
			integrationTestWrite(t, filepath.Join(f.installer.root, "agent_heavy.py"), []byte(source))
			slot, err := f.installer.legacySlot()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(expression, "older queue.slot") && slot != "/private/fixture/older queue.slot" {
				t.Fatalf("lost slot %s", slot)
			}
		})
	}
	for _, expression := range []string{`Path(run_arbitrary_code())`, `Path(os.environ.get(OVERRIDES[2], '/tmp/one'))\n        self.slot = Path('/tmp/two')`, `Path(os.environ.get(OVERRIDES[2], 'relative'))`, `Path(os.environ.get(OVERRIDES[2], '/tmp/\x00bad'))`} {
		f := installTestFixture(t)
		f.legacy(t)
		integrationTestWrite(t, filepath.Join(f.installer.root, "agent_heavy.py"), []byte("class Gate:\n    def __init__(self):\n        self.slot = "+expression+"\n"))
		if _, err := f.installer.legacySlot(); err == nil {
			t.Fatalf("unknown lease accepted: %s", expression)
		}
	}
}

func TestInstallerLeaseParserIgnoresStringsAndRequiresOwningConstructor(t *testing.T) {
	valid := `"""A module comment with a fake assignment:
self.slot = arbitrary()
"""
class Gate:
    def __init__(self):
        """self.slot = arbitrary()"""
        self.state = "unused"
        self.slot = lease_slot(self.state) # retained format
    def unrelated(self):
        self.slot = arbitrary()
`
	if expression, err := installSlotExpression(valid); err != nil || expression != "lease_slot(self.state)" {
		t.Fatalf("constructor lease: %s %v", expression, err)
	}
	for _, source := range []string{
		`class Other:
    def __init__(self):
        self.slot = lease_slot(self.state)
`,
		`class Gate:
    def unrelated(self):
        self.slot = lease_slot(self.state)
`,
		`class Gate:
    def __init__(self):
        """self.slot = lease_slot(self.state)"""
`,
		valid + `class Gate:
    def __init__(self):
        self.slot = lease_slot(self.state)
`,
	} {
		if expression, err := installSlotExpression(source); err == nil {
			t.Fatalf("accepted ambiguous lease: %s", expression)
		}
	}
}

func TestInstallerRecoversInterruptedAppRename(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	install := f.installer
	previous := filepath.Join(install.home, "Applications/.Agent Queue.previous.app")
	if err := os.Rename(install.app, previous); err != nil {
		t.Fatal(err)
	}
	f.fail = "kickstart -k"
	if err := f.run(t); err == nil {
		t.Fatal("simulated reload should fail")
	}
	if _, err := os.Stat(filepath.Join(install.app, "Contents/Info.plist")); err != nil {
		t.Fatalf("last usable app lost: %v", err)
	}
}

func TestInstallerCommandDeadlinesBoundServiceProbes(t *testing.T) {
	outer, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	for _, args := range [][]string{{"launchctl", "bootstrap", "fixture"}, {"ps", "-axo", "pid=,command="}} {
		bounded, release := installCommandContext(outer, args)
		actual, ok := bounded.Deadline()
		release()
		if !ok || time.Until(actual) > 30*time.Second {
			t.Fatalf("probe lacks short deadline: %v", args)
		}
	}
	short, cancelShort := context.WithTimeout(context.Background(), time.Second)
	defer cancelShort()
	bounded, release := installCommandContext(short, []string{"launchctl", "print", "fixture"})
	defer release()
	shortDeadline, _ := short.Deadline()
	actual, _ := bounded.Deadline()
	if !actual.Equal(shortDeadline) {
		t.Fatal("probe exceeded remaining operation deadline")
	}
}

func TestInstallerReadinessEnvironmentTargetsInstalledQueue(t *testing.T) {
	f := installTestFixture(t)
	install := f.installer
	ambiguous := map[string]string{
		"HOME":                     filepath.Join(t.TempDir(), "unrelated-user"),
		"AGENT_HEAVY_STATE":        filepath.Join(t.TempDir(), "unrelated-state"),
		"AGENT_HEAVY_CONFIG":       filepath.Join(t.TempDir(), "unrelated-config.yml"),
		"AGENT_HEAVY_LEGACY_SLOT":  filepath.Join(t.TempDir(), "unrelated-slot"),
		"AGENT_HEAVY_ACTIVE":       "unrelated-active-job",
		"PUEUE_CONFIG_PATH":        filepath.Join(t.TempDir(), "unrelated-pueue.yml"),
		"INSTALLER_OPERATOR_VALUE": "preserved-operator-value",
	}
	for key, value := range ambiguous {
		t.Setenv(key, value)
	}
	toMap := func(environment []string) map[string]string {
		result := map[string]string{}
		for _, entry := range environment {
			key, value, ok := strings.Cut(entry, "=")
			if !ok {
				t.Fatal("invalid command environment")
			}
			result[key] = value
		}
		return result
	}
	readiness := toMap(install.commandEnvironment([]string{filepath.Join(install.root, "bin/agent-heavy"), "status", "--json"}))
	for key, expected := range map[string]string{"HOME": install.home, "AGENT_HEAVY_STATE": install.state, "AGENT_HEAVY_CONFIG": filepath.Join(install.config, "pueue.yml"), "INSTALLER_OPERATOR_VALUE": ambiguous["INSTALLER_OPERATOR_VALUE"]} {
		if readiness[key] != expected {
			t.Fatalf("readiness %s escaped the installed target", key)
		}
	}
	for _, key := range []string{"AGENT_HEAVY_LEGACY_SLOT", "AGENT_HEAVY_ACTIVE", "PUEUE_CONFIG_PATH"} {
		if _, present := readiness[key]; present {
			t.Fatalf("unrelated readiness override survived: %s", key)
		}
	}
	general := toMap(install.commandEnvironment([]string{filepath.Join(install.root, "bin/agent-remote"), "migrate"}))
	for key, expected := range ambiguous {
		if general[key] != expected {
			t.Fatalf("unrelated command inheritance changed: %s", key)
		}
	}
}

func TestInstallerPreservesAppQuarantineMetadata(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS app metadata")
	}
	f := installTestFixture(t)
	install := f.installer
	install.testing = false
	source := filepath.Join(install.payload, "Agent Queue.app")
	quarantine := []byte("0081;6f35b100;AgentQueueInstaller;")
	if err := unix.Setxattr(source, "com.apple.quarantine", quarantine, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(source, "io.went.agent-queue.fixture", []byte("preserved app attribute"), 0); err != nil {
		t.Fatal(err)
	}
	command := install.command
	install.command = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "/usr/bin/ditto" {
			return exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		}
		return command(ctx, args)
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 1024)
	length, err := unix.Getxattr(install.app, "com.apple.quarantine", data)
	if err != nil {
		t.Fatal(err)
	}
	// macOS may canonicalize the quarantine record while retaining its flags.
	if !strings.HasPrefix(string(data[:length]), "0081;") {
		t.Fatalf("app quarantine was stripped: %q %v", data[:length], err)
	}
	length, err = unix.Getxattr(install.app, "io.went.agent-queue.fixture", data)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:length]) != "preserved app attribute" {
		t.Fatal("application extended attribute changed")
	}
}

func TestInstallerIdentitiesPreventDowngradeAndConflicts(t *testing.T) {
	for _, marker := range []string{"build-info.json", "install-in-progress.json", "app"} {
		for _, kind := range []string{"newer", "conflict", "corrupt", "symlink"} {
			t.Run(marker+"/"+kind, func(t *testing.T) {
				f := installTestFixture(t)
				install := f.installer
				info := metadataTestInfo(t, "124")
				if kind == "conflict" {
					info = metadataTestInfo(t, "123")
					commit := strings.Repeat("c", 40)
					info.Commit = &commit
				}
				filename := filepath.Join(install.root, marker)
				if marker == "app" {
					filename = filepath.Join(install.app, "Contents/Resources/build-info.json")
				}
				raw := mustMetadataJSON(t, info)
				if kind == "corrupt" {
					raw = []byte("invalid")
				}
				integrationTestWrite(t, filename, raw)
				if kind == "symlink" {
					if err := os.Rename(filename, filename+".target"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filename+".target", filename); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.run(t); err == nil {
					t.Fatal("unsafe identity accepted")
				}
				if _, err := os.Stat(filepath.Join(install.root, "bin/agent-heavy")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("preflight failure copied support")
				}
			})
		}
	}
}

func TestInstallerFailureLeavesIncompleteTransactionAndRetryRecovers(t *testing.T) {
	f := installTestFixture(t)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	install := f.installer
	previous := integrationTestRead(t, filepath.Join(install.root, "build-info.json"))
	info := metadataTestInfo(t, "124")
	integrationTestWrite(t, filepath.Join(install.payload, "source/build-info.json"), mustMetadataJSON(t, info))
	updateTestBundle(t, filepath.Join(install.payload, "Agent Queue.app"), "io.went.agent-queue", info, plist.XMLFormat)
	f.fail = "kickstart -k"
	if err := f.run(t); err == nil {
		t.Fatal("simulated reload succeeded")
	}
	if !bytes.Equal(integrationTestRead(t, filepath.Join(install.root, "build-info.json")), previous) {
		t.Fatal("failed upgrade advanced marker")
	}
	if _, err := updateInstalled(install.root, install.app); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("failed upgrade reported complete: %v", err)
	}
	f.fail = ""
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	installed, err := updateInstalled(install.root, install.app)
	if err != nil || installed["build"] != 124 {
		t.Fatalf("retry %v %v", installed, err)
	}
}

func TestInstallerLocksAndUnsafeDestinations(t *testing.T) {
	for _, kind := range []string{"install-lock", "scheduling-lock", "symlink-lock", "symlink-root", "symlink-app"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			install := f.installer
			if err := installDirectory(install.state); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "install-lock", "scheduling-lock":
				name := "install.lock"
				if kind == "scheduling-lock" {
					name = "scheduling.lock"
				}
				lock, err := installLock(install.state, name)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "symlink-lock":
				outside := filepath.Join(install.home, "outside")
				integrationTestWrite(t, outside, nil)
				if err := os.Symlink(outside, filepath.Join(install.state, "install.lock")); err != nil {
					t.Fatal(err)
				}
			case "symlink-root":
				outside := filepath.Join(install.home, "outside")
				if err := os.Mkdir(outside, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(install.root), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, install.root); err != nil {
					t.Fatal(err)
				}
			case "symlink-app":
				outside := filepath.Join(install.home, "outside")
				if err := os.Mkdir(outside, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(install.app), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, install.app); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.run(t); err == nil {
				t.Fatal("unsafe installation accepted")
			}
		})
	}
}

func TestInstallerRetirementRefusesSymlinkParentsAndWritableSource(t *testing.T) {
	for _, kind := range []string{"remote-parent", "venv", "writable-source"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			install := f.installer
			outside := filepath.Join(install.home, "outside")
			integrationTestWrite(t, filepath.Join(outside, "control.py"), []byte("unrelated private content"))
			switch kind {
			case "remote-parent":
				if err := os.Symlink(outside, filepath.Join(install.root, "remote")); err != nil {
					t.Fatal(err)
				}
			case "venv":
				if err := os.RemoveAll(filepath.Join(install.root, ".venv")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(install.root, ".venv")); err != nil {
					t.Fatal(err)
				}
			case "writable-source":
				integrationTestWrite(t, filepath.Join(install.root, "resource_monitor.py"), []byte("unsafe old source"))
				if err := os.Chmod(filepath.Join(install.root, "resource_monitor.py"), 0666); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.run(t); err == nil {
				t.Fatal("unsafe retirement accepted")
			}
			if string(integrationTestRead(t, filepath.Join(outside, "control.py"))) != "unrelated private content" {
				t.Fatal("unsafe retirement touched external files")
			}
			if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsafe retirement passed preflight")
			}
		})
	}
}

// The release smoke script supplies its extracted payload. This test exercises
// the actual universal binaries and native app distributed to users.
func TestInstallerPackagedPayload(t *testing.T) {
	installTestPackaged(t, false)
}

func TestInstallerPackagedAppBundle(t *testing.T) {
	installTestPackaged(t, true)
}

func installTestPackaged(t *testing.T, appBundle bool) {
	t.Helper()
	payload := os.Getenv("AGENT_QUEUE_TEST_INSTALLER_PAYLOAD")
	appSource := os.Getenv("AGENT_QUEUE_TEST_APP_BUNDLE")
	if payload == "" && (!appBundle || appSource == "") {
		t.Skip("release payload not supplied")
	}
	if payload != "" && !filepath.IsAbs(payload) {
		t.Fatal("release payload must be absolute")
	}
	f := installTestFixture(t)
	install := f.installer
	home, err := os.MkdirTemp("/tmp", "aq-pkg-")
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	install.home = home
	install.payload = payload
	install.root = filepath.Join(home, ".local/share/agent-heavy")
	install.state = filepath.Join(home, ".local/state/agent-heavy")
	install.config = filepath.Join(home, ".config/agent-heavy")
	install.launch = filepath.Join(home, "Library/LaunchAgents")
	install.app = filepath.Join(home, "Applications/Agent Queue.app")
	var appBefore map[string]installBundleEntry
	if appBundle {
		if appSource == "" {
			appSource = filepath.Join(payload, "Agent Queue.app")
		}
		if !filepath.IsAbs(appSource) {
			t.Fatal("release app must be absolute")
		}
		if err := os.Mkdir(filepath.Dir(install.app), 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		output, err := exec.CommandContext(ctx, "/usr/bin/ditto", "--rsrc", "--extattr", "--qtn", "--acl", appSource, install.app).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("copy packaged app into private home: %v: %s", err, output)
		}
		install.appBundle = true
		install.payload = filepath.Join(install.app, "Contents/Resources/payload")
		appBefore = installBundleSnapshot(t, install.app)
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(install.config, "pueue.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("packaged fresh install generated legacy queue configuration")
	}
	private := filepath.Join(install.state, "fixture-history")
	integrationTestWrite(t, private, []byte("preserved private fixture"))
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if string(integrationTestRead(t, private)) != "preserved private fixture" {
		t.Fatal("packaged upgrade changed configuration or private data")
	}
	for _, name := range []string{"agent_heavy.py", ".venv", "remote/server.py"} {
		if _, err := os.Lstat(filepath.Join(install.root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Python support unexpectedly installed: %s", name)
		}
	}
	environment := map[string]string{}
	for key, value := range envMap() {
		if !strings.HasPrefix(key, "AGENT_HEAVY_") && !strings.HasPrefix(key, "PUEUE_") {
			environment[key] = value
		}
	}
	environment["HOME"] = home
	environment["PATH"] = "/usr/bin:/bin:/usr/sbin:/sbin"
	environment["PACKAGED_TEST_VALUE"] = "fixture-env"
	childEnv := envList(environment)
	if appBundle {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		command := exec.CommandContext(ctx, filepath.Join(home, ".local/bin/agent-heavy"), "version", "--json")
		command.Env = childEnv
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("packaged self-installed version could not resolve app registration: %v: %s", err, output)
		}
		var version Object
		if err := json.Unmarshal(output, &version); err != nil || version["version"] != install.identity.Version {
			t.Fatalf("packaged self-installed version mismatch: %v %v", version, err)
		}
	}
	start := func(name string, args ...string) *exec.Cmd {
		t.Helper()
		log, err := os.Create(filepath.Join(home, name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(args[0], args[1:]...)
		command.Env = childEnv
		command.Stdout = log
		command.Stderr = log
		if err := command.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = command.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = command.Process.Kill()
				<-done
			}
			_ = log.Close()
		})
		return command
	}
	cli := filepath.Join(home, ".local/bin/agent-heavy")
	start("daemon", cli, "daemon")
	deadline := time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		command := exec.CommandContext(ctx, cli, "status", "--json")
		command.Env = childEnv
		err := command.Run()
		cancel()
		if err == nil {
			ready = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("packaged private daemon did not start: %s", integrationTestRead(t, filepath.Join(home, "daemon.log")))
	}
	if info, err := os.Lstat(QueueDatabasePath(install.state)); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("packaged queue database is not a private regular file: %v %v", info, err)
	}
	if appBundle {
		// Re-run setup with the real packaged daemon live, then exercise a job
		// through that same daemon and scheduler below.
		if err := f.run(t); err != nil {
			t.Fatalf("packaged app setup could not retry with a live daemon: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(install.config, "pueue.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("packaged upgrade or daemon generated legacy queue configuration")
	}
	start("scheduler", cli, "scheduler")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	job := exec.CommandContext(ctx, cli, "run", "--label", "Packaged fixture", "--", "/bin/sh", "-c", `printf '%s|%s|%s\n' "$1" "$2" "$PACKAGED_TEST_VALUE"; exit 7`, "packaged-fixture", "argument with spaces", "literal;$HOME")
	job.Env = childEnv
	output, err := job.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("packaged job exit %v: %s", err, output)
	}
	if !strings.Contains(string(output), "argument with spaces|literal;$HOME|fixture-env") {
		t.Fatalf("packaged argv/environment/output changed: %s", output)
	}
	var status Object
	telemetry := false
	for ctx.Err() == nil {
		command := exec.CommandContext(ctx, cli, "status", "--json")
		command.Env = childEnv
		raw, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("packaged status %v: %s", err, raw)
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			t.Fatalf("invalid packaged status %v: %s", err, raw)
		}
		scheduler := obj(status["scheduler"])
		host := obj(scheduler["host"])
		if host != nil {
			total, ok := number(host["totalMemoryBytes"])
			if ok && total > 0 {
				telemetry = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !telemetry {
		t.Fatalf("native telemetry unavailable: %v; scheduler log: %s", status, integrationTestRead(t, filepath.Join(home, "scheduler.log")))
	}
	if obj(status["queue"])["legacyOccupied"] != false {
		t.Fatal("completed packaged job retained its execution lease")
	}
	gate := &Gate{State: install.state, Slot: filepath.Join(install.state, "execution.slot")}
	fixtureUntil(t, 5*time.Second, func() bool {
		unlock, err := LockPrivate(gate, "")
		if err != nil {
			return false
		}
		defer unlock()
		_, err = os.Stat(gate.Slot)
		return errors.Is(err, os.ErrNotExist)
	})
	for _, args := range f.calls {
		if len(args) > 1 && args[0] == "launchctl" && args[1] == "bootout" {
			t.Fatal("Go upgrade restarted its daemon")
		}
	}
	if appBundle {
		if after := installBundleSnapshot(t, install.app); !reflect.DeepEqual(appBefore, after) {
			t.Fatal("packaged self-install or retry mutated the application")
		}
		assertInstallerNoAppCommands(t, f)
	}
}
