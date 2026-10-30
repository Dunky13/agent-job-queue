package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The menu bar app supervises the queue, scheduler and phone helpers itself, so
// app-bundle setup migrates their old Service Management registrations away and
// registers only the login item.

// Real launchctl output for a registration pinned to an older ad hoc build:
// launchd killed each new daemon, so it never held a process.
func managedCrashLoopOutput(domain string) []byte {
	return []byte(domain + "/io.went.agent-heavy = {\n" +
		"\tactive count = 0\n" +
		"\tpath = (submitted by smd.24419)\n" +
		"\ttype = Submitted\n" +
		"\tmanaged_by = com.apple.xpc.ServiceManagement\n" +
		"\tstate = spawn scheduled\n" +
		"\tprogram identifier = Contents/MacOS/agent-heavy (mode: 2)\n" +
		"\tparent bundle identifier = io.went.agent-queue\n" +
		"\tparent bundle version = 166\n" +
		"\targuments = {\n\t\tagent-heavy\n\t\tdaemon\n\t}\n" +
		"\truns = 25\n" +
		"\tlast exit code = 78: EX_CONFIG\n" +
		"}\n")
}

func TestManagedLoadedBundleProgramAcceptsCrashLoopWithoutProcess(t *testing.T) {
	executable := "/Applications/Agent Queue.app/Contents/MacOS/agent-heavy"
	raw := managedCrashLoopOutput("gui/502")
	if !managedLoadedBundleProgram(raw, executable, "agent-heavy", "daemon") {
		t.Fatal("a crash-looping registration with no process was not accepted for unregistration")
	}
	for _, test := range []struct {
		name, from, to string
	}{
		{"other bundle", "parent bundle identifier = io.went.agent-queue", "parent bundle identifier = io.example.other"},
		{"other helper", "program identifier = Contents/MacOS/agent-heavy", "program identifier = Contents/MacOS/agent-remote"},
		{"other command", "\t\tdaemon\n", "\t\tscheduler\n"},
		{"other submitter", "smd.24419", "smd.1"},
		{"running without a pid", "state = spawn scheduled", "state = running"},
		{"another program", "\tpath = (submitted by smd.24419)\n", "\tprogram = /another/helper\n\tpath = (submitted by smd.24419)\n"},
		{"no location at all", "\tpath = (submitted by smd.24419)\n", ""},
	} {
		if managedLoadedBundleProgram([]byte(strings.Replace(string(raw), test.from, test.to, 1)), executable, "agent-heavy", "daemon") {
			t.Errorf("%s: custody accepted", test.name)
		}
	}
	withProcess := strings.Replace(string(raw), "\tstate = spawn scheduled\n", "\tstate = running\n\tpid = "+fmt.Sprint(os.Getpid())+"\n", 1)
	if managedLoadedBundleProgram([]byte(withProcess), executable, "agent-heavy", "daemon") {
		t.Fatal("a live process running another executable was accepted")
	}
}

func managedAppBundleFixture(t *testing.T) *managedFixture {
	t.Helper()
	m := managedTestFixture(t)
	i := m.installer
	if err := installCopyTree(filepath.Join(i.payload, "Agent Queue.app"), i.app); err != nil {
		t.Fatal(err)
	}
	i.appBundle = true
	i.payload = filepath.Join(i.app, "Contents/Resources/payload")
	return m
}

func managedServiceCalls(m *managedFixture, action string) []string {
	services := []string{}
	for _, args := range m.calls {
		if len(args) == 4 && args[1] == "--service-management" && args[2] == action {
			services = append(services, args[3])
		}
	}
	return services
}

func managedSetupNotices(m *managedFixture) *[]string {
	notices := []string{}
	m.installer.notice = func(message string) { notices = append(notices, message) }
	return &notices
}

func TestManagedAppBundleRegistersOnlyTheLoginItem(t *testing.T) {
	m := managedAppBundleFixture(t)
	notices := managedSetupNotices(m)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	if got := managedServiceCalls(m, "register"); !slices.Equal(got, []string{"login"}) {
		t.Fatalf("registered %v, want only the login item", got)
	}
	if got := managedServiceCalls(m, "unregister"); len(got) != 0 {
		t.Fatalf("a fresh install unregistered %v", got)
	}
	for _, service := range []string{"queue", "resources", "remote"} {
		if m.statuses[service] != "notRegistered" {
			t.Fatalf("%s is %s after setup", service, m.statuses[service])
		}
	}
	if m.statuses["login"] != "enabled" {
		t.Fatalf("login is %s after setup", m.statuses["login"])
	}
	for _, args := range m.calls {
		if slices.Contains(args, "status") && slices.Contains(args, "--json") {
			t.Fatalf("setup waited on the daemon: %v", args)
		}
		if args[0] == "launchctl" && args[1] != "print" {
			t.Fatalf("setup changed launchd directly: %v", args)
		}
	}
	if len(*notices) != 0 {
		t.Fatalf("an approved login item produced notices: %v", *notices)
	}
	for _, name := range []string{"install-in-progress.json"} {
		if _, err := os.Lstat(filepath.Join(m.installer.root, name)); !os.IsNotExist(err) {
			t.Fatalf("setup left %s: %v", name, err)
		}
	}
	if _, err := installReadInfo(filepath.Join(m.installer.root, "build-info.json")); err != nil {
		t.Fatal("setup did not publish its build identity", err)
	}
}

func TestManagedAppBundleLoginApprovalNeverBlocksSetup(t *testing.T) {
	m := managedAppBundleFixture(t)
	notices := managedSetupNotices(m)
	m.approval = "login"
	if err := m.run(t); err != nil {
		t.Fatal("a login item awaiting approval blocked setup", err)
	}
	if m.statuses["login"] != "requiresApproval" {
		t.Fatalf("login is %s", m.statuses["login"])
	}
	if len(*notices) != 1 || strings.Contains((*notices)[0], "AGENT_QUEUE_SETUP_STATE") || !strings.Contains((*notices)[0], "approval") {
		t.Fatalf("expected one informational line without a state marker: %q", *notices)
	}
	if _, err := os.Lstat(filepath.Join(m.installer.root, "install-in-progress.json")); !os.IsNotExist(err) {
		t.Fatal("setup stayed pending behind the login approval", err)
	}
	// Opening the app again must also complete, without re-registering anything
	// the user has not approved or removing the pending login item.
	m.calls, *notices = nil, nil
	if err := m.run(t); err != nil {
		t.Fatal("retry with a pending login item failed", err)
	}
	if got := managedServiceCalls(m, "unregister"); len(got) != 0 {
		t.Fatalf("retry removed %v", got)
	}
	// Consent that was not granted is never requested a second time.
	if got := managedServiceCalls(m, "register"); len(got) != 0 {
		t.Fatalf("retry registered %v", got)
	}
	if len(*notices) != 1 {
		t.Fatalf("retry notices: %q", *notices)
	}
}

func TestManagedAppBundleRepairAndContinueCompletesWithPendingLogin(t *testing.T) {
	m := managedAppBundleFixture(t)
	managedSetupNotices(m)
	m.approval = "login"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if report := m.installer.preflight(ctx); len(report.Issues) != 0 {
		t.Fatalf("preflight blocked setup: %+v", report.Issues)
	}
	if err := m.installer.repairAndExecute(ctx); err != nil {
		t.Fatal("automatic setup waited for the login approval", err)
	}
	if m.statuses["login"] != "requiresApproval" {
		t.Fatalf("login is %s", m.statuses["login"])
	}
}

// An earlier version registered its helpers with Service Management. Upgrading
// retires every one of them, including a registration that never got approved.
func TestManagedAppBundleMigratesOldRegistrationsAwaitingApproval(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	for _, service := range []string{"queue", "resources", "remote"} {
		m.statuses[service] = "requiresApproval"
	}
	clear(m.loaded)
	m.calls = nil
	managedSetupNotices(m)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if report := i.preflight(ctx); len(report.Issues) != 0 {
		t.Fatalf("an old registration awaiting approval blocked setup: %+v", report.Issues)
	}
	if err := m.run(t); err != nil {
		t.Fatal("an old registration awaiting approval blocked the upgrade", err)
	}
	got := managedServiceCalls(m, "unregister")
	slices.Sort(got)
	// The login item is re-registered after the upgrade, so it is retired too.
	if !slices.Equal(got, []string{"login", "queue", "remote", "resources"}) {
		t.Fatalf("unregistered %v", got)
	}
	for _, service := range []string{"queue", "resources", "remote"} {
		if m.statuses[service] != "notRegistered" {
			t.Fatalf("%s is %s after upgrade", service, m.statuses[service])
		}
	}
	if got := managedServiceCalls(m, "register"); !slices.Equal(got, []string{"login"}) {
		t.Fatalf("registered %v, want only the login item", got)
	}
}

func TestManagedAppBundleUnregistersCrashLoopingRegistrationWithoutProcess(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	// Launchd keeps the registration but never runs it; nothing holds a lock.
	m.statuses["queue"] = "enabled"
	m.loaded["io.went.agent-heavy"] = true
	delete(m.loaded, "io.went.agent-resources")
	m.statuses["resources"] = "notRegistered"
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[0] == "launchctl" && args[1] == "print" && args[2] == i.domain()+"/io.went.agent-heavy" && m.loaded["io.went.agent-heavy"] {
			m.calls = append(m.calls, slices.Clone(args))
			return managedCrashLoopOutput(i.domain()), nil
		}
		return original(ctx, args)
	}
	m.calls = nil
	managedSetupNotices(m)
	if err := m.run(t); err != nil {
		t.Fatal("a crash-looping registration blocked the retry", err)
	}
	// The login item is re-registered after the upgrade, so it is retired too.
	if got := managedServiceCalls(m, "unregister"); !slices.Equal(got, []string{"queue", "login"}) {
		t.Fatalf("unregistered %v", got)
	}
	if m.loaded["io.went.agent-heavy"] || m.statuses["queue"] != "notRegistered" {
		t.Fatal("the stale registration remained", m.statuses)
	}
	if got := managedServiceCalls(m, "register"); !slices.Equal(got, []string{"login"}) {
		t.Fatalf("registered %v, want only the login item", got)
	}
}

func TestManagedAppBundleRefusesRegistrationWhoseProcessIsNotThisHelper(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	m.statuses["queue"] = "enabled"
	m.loaded["io.went.agent-heavy"] = true
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[0] == "launchctl" && args[1] == "print" && args[2] == i.domain()+"/io.went.agent-heavy" && m.loaded["io.went.agent-heavy"] {
			m.calls = append(m.calls, slices.Clone(args))
			// A live process whose executable is the test binary, not the helper.
			output := strings.Replace(string(managedCrashLoopOutput(i.domain())), "\tstate = spawn scheduled\n", fmt.Sprintf("\tstate = running\n\tpid = %d\n", os.Getpid()), 1)
			return []byte(output), nil
		}
		return original(ctx, args)
	}
	before := installTreeWitness(t, i.home)
	m.calls = nil
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "differs from the canonical managed helper") {
		t.Fatal("a live process outside this app's helper was unregistered", err)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("refusal changed the installation")
	}
	managedRequireNoServiceMutation(t, m.calls)
}

func TestManagedAppBundlePreflightIgnoresOldRegistrations(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	for _, status := range []string{"requiresApproval", "enabled", "notRegistered"} {
		for _, service := range []string{"queue", "resources", "remote", "login"} {
			m.statuses[service] = status
		}
		clear(m.loaded)
		report := i.preflight(context.Background())
		if len(report.Issues) != 0 {
			t.Fatalf("%s registrations blocked setup: %+v", status, report.Issues)
		}
	}
}

func TestManagedAppBundlePreflightAcceptsLoadedCrashLoopingRegistration(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	i.appBundle, i.payload = true, filepath.Join(i.app, "Contents/Resources/payload")
	m.statuses["queue"] = "enabled"
	m.loaded["io.went.agent-heavy"] = true
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[0] == "launchctl" && args[1] == "print" && args[2] == i.domain()+"/io.went.agent-heavy" && m.loaded["io.went.agent-heavy"] {
			return managedCrashLoopOutput(i.domain()), nil
		}
		return original(ctx, args)
	}
	before := installTreeWitness(t, i.home)
	m.calls = nil
	if report := i.preflight(context.Background()); len(report.Issues) != 0 {
		t.Fatalf("a crash-looping registration blocked setup: %+v", report.Issues)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("preflight changed the installation")
	}
	managedRequireNoServiceMutation(t, m.calls)
}
