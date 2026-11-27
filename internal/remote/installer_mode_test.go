package remote

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
)

// Every command is replaced with a recorder, so a missing guard fails this test
// without reaching Service Management or launchd.
func TestInstallerTestModeRefusesServiceControl(t *testing.T) {
	for name, fixture := range map[string]func(*testing.T) *controller{"managed": fixtureManagedController, "unmanaged": fixtureController} {
		t.Run(name, func(t *testing.T) {
			c := fixture(t)
			enterInstallerTestMode(t, c)
			var commands []string
			c.newCommand = func(ctx context.Context, command string, args ...string) *exec.Cmd {
				commands = append(commands, command+" "+strings.Join(args, " "))
				return exec.CommandContext(ctx, "/usr/bin/true")
			}
			for _, action := range []string{"stop", "start"} {
				if err := c.serviceAction(action); err == nil || !strings.Contains(err.Error(), "Installer test mode") {
					t.Error("installer test mode allowed service control", action, err)
				}
			}
			if len(commands) != 0 {
				t.Fatal("installer test mode ran service commands", commands)
			}
		})
	}
}

// The whole controller command, with real process execution: the recorded app's
// AgentQueue only logs its arguments and reports no registration, which keeps a
// missing guard on the managed route and away from launchctl.
func TestInstallerTestModeControllerNeverRunsAgentQueue(t *testing.T) {
	for _, action := range []string{"disable", "migrate"} {
		t.Run(action, func(t *testing.T) {
			c := fixtureManagedController(t)
			record := filepath.Join(c.p.home, "service-management.argv")
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$HOME/service-management.argv\"\n" +
				"printf '{\"schemaVersion\":1,\"service\":\"remote\",\"status\":\"notRegistered\"}\\n'\n"
			if err := os.WriteFile(filepath.Join(c.p.managedApp, "Contents/MacOS/AgentQueue"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			enterInstallerTestMode(t, c)
			code, output := runMainOutput(t, action)
			if raw, err := os.ReadFile(record); !os.IsNotExist(err) {
				t.Fatalf("installer test mode reached Service Management: %v: %s", err, raw)
			}
			if code != 125 || !strings.Contains(output, "Installer test mode") {
				t.Fatalf("%s was not refused: exit %d: %s", action, code, output)
			}
			if _, err := os.Lstat(c.p.errorFile); !os.IsNotExist(err) {
				t.Fatal("refusal wrote the failure state", err)
			}
		})
	}
}

// The recorded supervisor and service child are processes this test owns, so a
// missing guard signals only them. runMain builds its own controller, which
// leaves no place for fake signal functions.
func TestInstallerTestModeLeavesSettingsAndProcesses(t *testing.T) {
	token := strings.Repeat("a", 32)
	for _, arguments := range [][]string{{"configure"}, {"enable"}, {"disable"}, {"migrate"}, {"serve"}, {"child", "api", token}} {
		t.Run(arguments[0], func(t *testing.T) {
			c := fixtureController(t)
			enterInstallerTestMode(t, c)
			if err := c.p.prepare(); err != nil {
				t.Fatal(err)
			}
			settings := testSettings()
			// A missing guard must not start a real supervisor from the test binary.
			settings.Enabled = arguments[0] != "serve"
			boot, err := c.bootFn()
			if err != nil {
				t.Fatal(err)
			}
			var owned []processRecord
			for range 2 {
				process := exec.Command("/bin/sleep", "60")
				process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				if err := process.Start(); err != nil {
					t.Fatal(err)
				}
				exited := make(chan struct{})
				go func() { _ = process.Wait(); close(exited) }()
				t.Cleanup(func() { _ = process.Process.Kill(); <-exited })
				record, err := c.record(process.Process.Pid)
				if err != nil {
					t.Fatal(err)
				}
				owned = append(owned, record)
			}
			child, err := c.p.childPath(token, "api")
			if err != nil {
				t.Fatal(err)
			}
			for path, value := range map[string]any{
				c.p.config: settings,
				c.p.pid:    generationRecord{Token: token, Active: true, BootUUID: boot, Supervisor: owned[0]},
				child:      childRecord{PID: owned[1].PID, Birth: owned[1].Birth, Generation: token, Role: "api", BootUUID: boot},
			} {
				if err := writePrivate(path, value); err != nil {
					t.Fatal(err)
				}
			}
			before := homeContents(t, c.p.home)
			code, output := runMainOutput(t, arguments...)
			if code != 125 || !strings.Contains(output, "Installer test mode") {
				t.Errorf("not refused: exit %d: %s", code, output)
			}
			for _, record := range owned {
				if birth, err := c.birthFn(record.PID); err != nil || birth != record.Birth {
					t.Error("installer test mode signalled a recorded process", record.PID, err)
				}
			}
			if after := homeContents(t, c.p.home); !maps.Equal(before, after) {
				t.Errorf("installer test mode changed the home:\n%v\n%v", before, after)
			}
		})
	}
}

func runMainOutput(t *testing.T, arguments ...string) (int, string) {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "remote-stdout")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = output
	code := runMain(arguments, strings.NewReader(""), prepareConfiguration, defaultLifecycleContext)
	os.Stdout = original
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(raw)
}

// homeContents maps every entry under home to its mode and file contents.
func homeContents(t *testing.T, home string) map[string]string {
	t.Helper()
	contents := map[string]string{}
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var raw []byte
		if info.Mode().IsRegular() {
			if raw, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		contents[path] = info.Mode().String() + " " + string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

// enterInstallerTestMode gives c the paths agent-remote resolves when the
// installer runs it: its own home, no AGENT_REMOTE_* and the installer signal.
// It stops the test unless the fixture's app is the recorded owner.
func enterInstallerTestMode(t *testing.T, c *controller) {
	t.Helper()
	app := c.p.managedApp
	if app != "" {
		metadata := filepath.Join(c.p.home, ".local/share/agent-heavy/installation.json")
		if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writePrivate(metadata, appservices.Installation{SchemaVersion: 1, App: app, ServiceManagement: true}); err != nil {
			t.Fatal(err)
		}
		if err := appservices.ValidateApp(c.p.home, app); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", c.p.home)
	for _, name := range []string{"AGENT_REMOTE_TEST_MODE", "AGENT_REMOTE_STATE", "AGENT_REMOTE_CONFIG"} {
		t.Setenv(name, "")
	}
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_MODE", "1")
	p, err := newPaths()
	if err != nil || p.testing || p.managedApp != app {
		t.Fatal("fixture does not resolve like the installer's helper", err, p.testing, p.managedApp)
	}
	c.p = p
}
