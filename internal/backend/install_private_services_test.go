package backend

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestManagedPrivateNativeCommandsCannotReachRealServices(t *testing.T) {
	// Unix-domain socket paths on macOS cannot fit Go's long test-name roots.
	root, err := os.MkdirTemp("/tmp", "agent-queue-install-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	home, tools := filepath.Join(root, "User With Spaces"), filepath.Join(root, "tools")
	for _, directory := range []string{home, tools} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	i := &installer{home: home, app: filepath.Join(home, "Applications/Agent Queue.app"), state: filepath.Join(home, ".local/state/agent-heavy"), config: filepath.Join(home, ".config/agent-heavy"), path: tools, testing: true}
	args := []string{filepath.Join(i.app, "Contents/MacOS/AgentQueue"), "--service-management", "status", "queue"}
	// Even before a native executable exists, missing explicit fixture custody
	// must produce the isolation error rather than attempting that executable.
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_MANAGED", "")
	if _, err := i.run(context.Background(), args...); err == nil || !strings.Contains(err.Error(), "real service management is disabled") {
		t.Fatal("test mode could reach the real native path", err)
	}
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_MANAGED", "private-native-lifecycle")
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_TOOLS", tools)
	t.Setenv("AGENT_HEAVY_STATE", "/foreign/queue")
	tool := filepath.Join(tools, "service-management")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" \"$2\" \"$HOME\" \"$AGENT_HEAVY_STATE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "native.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	output, err := i.run(context.Background(), args...)
	if want := strings.Join([]string{"status", "queue", home, i.state, ""}, "\n"); err != nil || string(output) != want {
		t.Fatalf("private lifecycle tool lost argument or environment isolation: %q: %v", output, err)
	}
	replacement := []string{args[0], "--prepare-app-replacement", i.app}
	output, err = i.run(context.Background(), replacement...)
	if want := strings.Join([]string{"prepare-app-replacement", i.app, home, i.state, ""}, "\n"); err != nil || string(output) != want {
		t.Fatalf("private UI shutdown tool lost argument or environment isolation: %q: %v", output, err)
	}
	if _, err := i.privateManagedCommand([]string{args[0], "--prepare-app-replacement", filepath.Join(home, "Other.app")}); err == nil {
		t.Fatal("private fixture accepted another app's UI shutdown")
	}
	for _, target := range []string{root, home, tools, tool, socket} {
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, info.Mode().Perm()|0020); err != nil {
			t.Fatal(err)
		}
		_, commandErr := i.privateManagedCommand(args)
		if err := os.Chmod(target, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		if commandErr == nil {
			t.Fatal("accepted unsafe fixture custody", target)
		}
	}
	for _, change := range [][]string{
		{filepath.Join(home, "Another.app/Contents/MacOS/AgentQueue"), args[1], args[2], args[3]},
		{args[0], args[1], "unknown", args[3]},
		{args[0], args[1], args[2], "unknown"},
		append(slices.Clone(args), "extra"),
	} {
		if _, err := i.privateManagedCommand(change); err == nil {
			t.Fatal("accepted foreign native command", change)
		}
	}
}

func TestManagedInstallerOpensAppOnlyAfterDurableCompletion(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	i.testing = false
	original := i.command
	opened := false
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "/usr/bin/ditto" {
			return nil, installCopyTree(args[len(args)-2], args[len(args)-1])
		}
		if args[0] == "/usr/bin/open" {
			if !slices.Equal(args, []string{"/usr/bin/open", "-a", i.app}) {
				t.Fatal("opened an unrelated application", args)
			}
			if _, err := os.Stat(filepath.Join(i.root, "install-in-progress.json")); !os.IsNotExist(err) {
				t.Fatal("opened ordinary app before setup completion", err)
			}
			if _, err := updateInstalled(i.root, i.app); err != nil {
				t.Fatal("opened app without completed matching identity", err)
			}
			opened = true
			return nil, nil
		}
		return original(ctx, args)
	}
	if err := m.run(t); err != nil || !opened {
		t.Fatal("explicit managed install did not open the ready app", err)
	}
}
