package backend

import (
	"context"
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
	if m.statuses["login"] != "enabled" || m.statuses["queue"] != "notRegistered" || m.statuses["resources"] != "notRegistered" || m.statuses["remote"] != "notRegistered" {
		t.Fatal("adopted bundle registered more than its login item", m.statuses)
	}
}

func TestManagedAppBundleRelocatesAnExistingCompletedApp(t *testing.T) {
	m, old := managedDraggedFixture(t, false)
	i := m.installer
	if err := installCopyTree(i.app, old); err != nil {
		t.Fatal(err)
	}
	if err := m.run(t); err != nil {
		t.Fatal("setup could not relocate the verified completed app", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("relocation did not preserve the old app", err)
	}
}

func TestManagedAppBundleKeepsRecordOfMissingRunningApp(t *testing.T) {
	m, _ := managedDraggedFixture(t, false)
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
		{"running submitted helper", append([]byte("\tpath = (submitted by smd.99177)\n"), output("Contents/MacOS/agent-heavy", "io.went.agent-queue", pid, "agent-heavy", "daemon")...), self, true},
		{"stopped submitted helper owns no process to hijack", append([]byte("\tpath = (submitted by smd.99177)\n"), []byte(strings.Replace(string(output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "", "agent-heavy", "daemon")), "\tstate = running", "\tstate = not running", 1))...), self, true},
		{"submitted other executable", append([]byte("\tpath = (submitted by smd.99177)\n"), output("Contents/MacOS/agent-heavy", "io.went.agent-queue", pid, "agent-heavy", "daemon")...), "/Applications/Agent Queue.app/Contents/MacOS/agent-heavy", false},
		{"invalid submitted descriptor", append([]byte("\tpath = (submitted by smd.invalid)\n"), output("Contents/MacOS/agent-heavy", "io.went.agent-queue", pid, "agent-heavy", "daemon")...), self, false},
		{"duplicate submitted path", append([]byte("\tpath = (submitted by smd.99177)\n\tpath = (submitted by smd.99177)\n"), output("Contents/MacOS/agent-heavy", "io.went.agent-queue", pid, "agent-heavy", "daemon")...), self, false},
		{"stopped helper without location", []byte(strings.Replace(string(output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "", "agent-heavy", "daemon")), "\tstate = running", "\tstate = not running", 1)), "/Applications/Agent Queue.app/Contents/MacOS/agent-heavy", false},
		{"other executable", output("Contents/MacOS/agent-heavy", "io.went.agent-queue", pid, "agent-heavy", "daemon"), "/Applications/Agent Queue.app/Contents/MacOS/agent-heavy", false},
		{"other bundle", output("Contents/MacOS/agent-heavy", "io.example.other", "", "agent-heavy", "daemon"), self, false},
		{"other helper", output("Contents/MacOS/agent-remote", "io.went.agent-queue", "", "agent-heavy", "daemon"), self, false},
		{"other command", output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "", "agent-heavy", "scheduler"), self, false},
		{"pid zero", output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "0", "agent-heavy", "daemon"), self, false},
		{"pid one", output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "1", "agent-heavy", "daemon"), self, false},
		{"malformed pid", output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "abc", "agent-heavy", "daemon"), self, false},
		{"two pids", append(output("Contents/MacOS/agent-heavy", "io.went.agent-queue", pid, "agent-heavy", "daemon"), []byte("\tpid = "+pid+"\n")...), self, false},
		{"two identifiers", append(output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "", "agent-heavy", "daemon"), []byte("\tprogram identifier = Contents/MacOS/agent-heavy (mode: 2)\n")...), self, false},
		{"missing arguments", []byte("\tprogram identifier = Contents/MacOS/agent-heavy (mode: 2)\n\tparent bundle identifier = io.went.agent-queue\n"), self, false},
		{"running without pid", output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "", "agent-heavy", "daemon"), self, false},
		{"contradictory program", append([]byte("\tprogram = /another/helper\n"), output("Contents/MacOS/agent-heavy", "io.went.agent-queue", pid, "agent-heavy", "daemon")...), self, false},
		{"contradictory stopped program", append([]byte("\tprogram = /another/helper\n"), []byte(strings.Replace(string(output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "", "agent-heavy", "daemon")), "\tstate = running", "\tstate = spawn scheduled", 1))...), self, false},
		{"stopped without pid or location", []byte(strings.Replace(string(output("Contents/MacOS/agent-heavy", "io.went.agent-queue", "", "agent-heavy", "daemon")), "\tstate = running", "\tstate = spawn scheduled", 1)), self, false},
	} {
		if got := managedLoadedBundleProgram(test.raw, test.executable, "agent-heavy", "daemon"); got != test.want {
			t.Errorf("%s: custody %v, want %v", test.name, got, test.want)
		}
	}
}

func TestManagedStoppedBundleProgramRequiresCanonicalLocation(t *testing.T) {
	app := "/Applications/Agent Queue.app"
	executable := filepath.Join(app, "Contents/MacOS/agent-heavy")
	plist := filepath.Join(app, "Contents/Library/LaunchAgents/io.went.agent-heavy.plist")
	raw := "\tstate = spawn scheduled\n\tprogram identifier = Contents/MacOS/agent-heavy (mode: 2)\n\tparent bundle identifier = io.went.agent-queue\n\targuments = {\n\t\tagent-heavy\n\t\tdaemon\n\t}\n"
	for _, test := range []struct {
		name, location string
		want           bool
	}{
		{"canonical program", "\tprogram = " + executable + "\n", true},
		{"canonical definition", "\tpath = " + plist + "\n", true},
		{"other program", "\tprogram = /other/helper\n", false},
		{"other definition", "\tpath = /other/app/Contents/Library/LaunchAgents/io.went.agent-heavy.plist\n", false},
		{"contradictory definition", "\tprogram = " + executable + "\n\tpath = /other/definition.plist\n", false},
		{"duplicate definition", "\tpath = " + plist + "\n\tpath = " + plist + "\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := managedLoadedBundleProgram([]byte(raw+test.location), executable, "agent-heavy", "daemon"); got != test.want {
				t.Fatalf("custody %v, want %v", got, test.want)
			}
		})
	}
}

func TestManagedAppBundleNewerBuildSupersedesInterruptedSetup(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	marker := filepath.Join(i.root, "install-in-progress.json")
	// Finder replaced the app while setup for build 124 was interrupted.
	if err := installAtomic(marker, mustMetadataJSON(t, metadataTestInfo(t, "124")), 0600); err != nil {
		t.Fatal(err)
	}
	drop := func(build string) {
		t.Helper()
		managedCandidate(t, m, build)
		if err := os.RemoveAll(i.app); err != nil {
			t.Fatal(err)
		}
		if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
			t.Fatal(err)
		}
	}
	payload := i.payload
	drop("123")
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.run(t); err == nil {
		t.Fatal("an older bundle superseded a newer interrupted setup")
	}
	if after, err := os.ReadFile(marker); err != nil || string(after) != string(before) {
		t.Fatal("refusal changed the interrupted setup marker", err)
	}
	i.appBundle, i.payload = false, payload
	drop("125")
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	if err := m.run(t); err != nil {
		t.Fatal("a newer dropped bundle could not supersede interrupted setup", err)
	}
	completed, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || completed.Build == nil || *completed.Build != 125 {
		t.Fatal("newer bundle did not complete setup", completed, err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed setup retained the interrupted marker", err)
	}
}

func TestManagedAppBundlePlanningReadsStoppedStoreWhenDaemonIsUnreachable(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	store, err := openQueueStore(i.state, true)
	if err != nil {
		t.Fatal(err)
	}
	store.close()
	i.queueSnapshot = func(context.Context) (Object, error) { return nil, errors.New("queue daemon temporarily unavailable") }
	ctx := context.Background()
	if _, err := i.planManaged(ctx); err != nil {
		t.Fatal("a drained stopped store did not authorize planning", err)
	}
	held, err := installLock(i.state, "queue-daemon.lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.planManaged(ctx); err == nil || !strings.Contains(err.Error(), "Cannot verify the installed queue is drained") {
		t.Fatal("a daemon holding its lifetime lock was bypassed", err)
	}
	held.Close()
	i.appBundle, i.payload = false, filepath.Join(filepath.Dir(i.payload), "unused")
	if _, err := i.planManaged(ctx); err == nil {
		t.Fatal("payload setup used the stopped-store fallback")
	}
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	store, err = openQueueStore(i.state, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(41,'fixture','{}','running')")
	store.close()
	if err != nil {
		t.Fatal(err)
	}
	// The app stopped its daemon when it quit, and only the app can restart it
	// to recover these jobs, so the supervised path never waits for a drain.
	if _, err := i.planManaged(ctx); err != nil {
		t.Fatal("an unfinished stopped job blocked supervised planning", err)
	}
	i.appBundle, i.payload = false, filepath.Join(filepath.Dir(i.payload), "unused")
	if _, err := i.planManaged(ctx); err == nil {
		t.Fatal("payload setup accepted an unfinished stopped job")
	}
}

func TestManagedAppBundleKeepsCustodyThatStillGuardsABundle(t *testing.T) {
	t.Run("backup beside deleted app", func(t *testing.T) {
		m, old := managedDraggedFixture(t, false)
		if err := os.MkdirAll(filepath.Join(filepath.Dir(old), ".Agent Queue.previous.app"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := m.run(t); err == nil {
			t.Fatal("a deleted app with a retained backup was adopted")
		}
		managedRequireNoServiceMutation(t, m.calls)
	})
	t.Run("receipt with pending setup", func(t *testing.T) {
		m, _ := managedDraggedFixture(t, true)
		i := m.installer
		identity, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := installAtomic(filepath.Join(i.root, "install-in-progress.json"), mustMetadataJSON(t, identity), 0600); err != nil {
			t.Fatal(err)
		}
		if err := m.run(t); err == nil {
			t.Fatal("a receipt for pending setup was treated as superseded")
		}
		if _, err := os.Lstat(i.managedReceiptPath()); err != nil {
			t.Fatal("pending publication receipt was not preserved", err)
		}
	})
	t.Run("newer build with retained receipt", func(t *testing.T) {
		m := managedTestFixture(t)
		if err := m.run(t); err != nil {
			t.Fatal(err)
		}
		i := m.installer
		marker := filepath.Join(i.root, "install-in-progress.json")
		if err := installAtomic(marker, mustMetadataJSON(t, metadataTestInfo(t, "124")), 0600); err != nil {
			t.Fatal(err)
		}
		integrationTestWrite(t, i.managedReceiptPath(), []byte("{}"))
		managedCandidate(t, m, "125")
		if err := os.RemoveAll(i.app); err != nil {
			t.Fatal(err)
		}
		if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
			t.Fatal(err)
		}
		i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
		if superseded, err := i.managedPendingSuperseded(metadataTestInfo(t, "125"), metadataTestInfo(t, "124")); err != nil || superseded {
			t.Fatal("a retained receipt did not keep the pending build authoritative", superseded, err)
		}
	})
}
