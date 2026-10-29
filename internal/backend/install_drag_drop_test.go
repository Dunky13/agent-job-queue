package backend

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
)

// managedDraggedFixture completes a managed install, then simulates Finder
// deleting that app and the user dropping a fresh copy at another location.
func managedDraggedFixture(t *testing.T, receipt bool) (*managedFixture, string) {
	t.Helper()
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	old := i.app
	if receipt {
		identity, err := managedCaptureBundle(old)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(managedPublicationReceipt{SchemaVersion: 1, App: old, Pending: identity.Build, Candidate: identity})
		if err != nil {
			t.Fatal(err)
		}
		if err := installAtomic(i.managedReceiptPath(), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for service := range m.statuses {
		m.statuses[service] = "notRegistered"
	}
	clear(m.loaded)
	dropped := filepath.Join(filepath.Dir(old), "Agent Queue copy.app")
	if err := os.Rename(old, dropped); err != nil {
		t.Fatal(err)
	}
	i.app, i.appBundle, i.payload = dropped, true, filepath.Join(dropped, "Contents/Resources/payload")
	m.calls = nil
	return m, old
}

func TestManagedAppBundleAdoptsLocationRecordOfDeletedApp(t *testing.T) {
	m, _ := managedDraggedFixture(t, true)
	i := m.installer
	if err := m.run(t); err != nil {
		t.Fatal("a drag-and-drop copy could not replace a deleted app's record", err)
	}
	metadata, err := appservices.Load(i.home)
	if err != nil || metadata == nil || metadata.App != i.app {
		t.Fatalf("location record %v: %v", metadata, err)
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		raw, err := os.ReadFile(filepath.Join(i.home, ".local/bin", name))
		if err != nil || string(raw) != string(i.managedWrapper(name)) {
			t.Fatalf("CLI wrapper %s still names the deleted app: %q %v", name, raw, err)
		}
	}
	if _, err := os.Lstat(i.managedReceiptPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("superseded publication receipt was retained", err)
	}
	if m.statuses["queue"] != "enabled" || m.statuses["resources"] != "enabled" {
		t.Fatal("adopted bundle did not register its services", m.statuses)
	}
}

func TestManagedAppBundleKeepsRecordOfExistingOrRunningApp(t *testing.T) {
	m, old := managedDraggedFixture(t, false)
	i := m.installer
	if err := installCopyTree(i.app, old); err != nil {
		t.Fatal(err)
	}
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "location changed") {
		t.Fatal("setup adopted a new location while the recorded app still exists", err)
	}
	managedRequireNoServiceMutation(t, m.calls)
	if err := os.RemoveAll(old); err != nil {
		t.Fatal(err)
	}
	m.statuses["queue"] = "enabled"
	m.loaded["io.went.agent-heavy"] = true
	if err := m.run(t); err == nil {
		t.Fatal("setup adopted a new location while a service still ran from the recorded app")
	}
	managedRequireNoServiceMutation(t, m.calls)
}

func TestManagedLoadedBundleProgramRequiresThisHelper(t *testing.T) {
	self, err := installProcessExecutable(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	output := func(identifier, bundle, pid string, arguments ...string) []byte {
		text := "gui/501/io.went.agent-heavy = {\n\tstate = running\n\n\tprogram identifier = " + identifier + " (mode: 2)\n" +
			"\tparent bundle identifier = " + bundle + "\n\targuments = {\n\t\t" + strings.Join(arguments, "\n\t\t") + "\n\t}\n"
		if pid != "" {
			text += "\tpid = " + pid + "\n"
		}
		return []byte(text + "\tendpoints = {\n\t\tpid = 1\n\t}\n}\n")
	}
	pid := strconv.Itoa(os.Getpid())
	for _, test := range []struct {
		name       string
		raw        []byte
		executable string
		want       bool
	}{
		{"running helper", output("Contents/MacOS/agent-heavy", "io.went.agent-queue", pid, "agent-heavy", "daemon"), self, true},
		{"stopped helper", output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "", "agent-heavy", "daemon"), "/Applications/Agent Queue.app/Contents/MacOS/agent-heavy", true},
		{"other executable", output("Contents/MacOS/agent-heavy", "io.went.agent-queue", pid, "agent-heavy", "daemon"), "/Applications/Agent Queue.app/Contents/MacOS/agent-heavy", false},
		{"other bundle", output("Contents/MacOS/agent-heavy", "io.example.other", "", "agent-heavy", "daemon"), self, false},
		{"other helper", output("Contents/MacOS/agent-remote", "io.went.agent-queue", "", "agent-heavy", "daemon"), self, false},
		{"other command", output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "", "agent-heavy", "scheduler"), self, false},
	} {
		if got := managedLoadedBundleProgram(test.raw, test.executable, "agent-heavy", "daemon"); got != test.want {
			t.Errorf("%s: custody %v, want %v", test.name, got, test.want)
		}
	}
}
