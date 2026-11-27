package remote

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
			code := runMain([]string{action}, strings.NewReader(""), prepareConfiguration, defaultLifecycleContext)
			if raw, err := os.ReadFile(record); !os.IsNotExist(err) {
				t.Fatalf("installer test mode reached Service Management: %v: %s", err, raw)
			}
			failure, err := os.ReadFile(c.p.errorFile)
			if code != 125 || err != nil || !strings.Contains(string(failure), "Installer test mode") {
				t.Fatalf("%s was not refused: exit %d: %v: %s", action, code, err, failure)
			}
		})
	}
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
