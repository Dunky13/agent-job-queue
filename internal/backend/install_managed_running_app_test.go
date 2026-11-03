package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fixtureSupervisingApp = "fixture-supervising-app"
const fixturePhoneService = "fixture-phone-service"

// fixtureSuperviseHelpers stands in for the menu bar app: it starts the bundle's
// real helpers as its own children and stops them before it exits.
func fixtureSuperviseHelpers(helper string, phone ...string) int {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	children := []*exec.Cmd{}
	commands := [][]string{{helper, "daemon"}, {helper, "scheduler"}}
	if len(phone) == 2 {
		commands = append(commands, []string{phone[0], fixturePhoneService, phone[1]})
	}
	for _, command := range commands {
		child := exec.Command(command[0], command[1:]...)
		child.Stdout, child.Stderr = os.Stderr, os.Stderr
		if err := child.Start(); err != nil {
			return 1
		}
		children = append(children, child)
	}
	<-stop
	for _, child := range children {
		_ = child.Process.Signal(syscall.SIGTERM)
	}
	for _, child := range children {
		_ = child.Wait()
	}
	return 0
}

// fixtureHoldPhoneService holds what a running phone service holds: its lifetime
// lock and an active ownership record, which it clears when asked to stop.
func fixtureHoldPhoneService(state string) int {
	lock, err := installLock(state, "supervisor.lock")
	if err != nil {
		return 1
	}
	defer lock.Close()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	record := filepath.Join(state, "processes.json")
	if err := WritePrivate(record, Object{"active": true}); err != nil {
		return 1
	}
	<-stop
	if err := WritePrivate(record, Object{"active": false}); err != nil {
		return 1
	}
	return 0
}

// runningApp is a real process tree holding real locks. Only Service Management,
// codesign and the native quit request are mocked; lsof, the kernel's process
// records and the daemon's RPC are the real ones.
type runningApp struct {
	*managedFixture
	helper   string
	process  *exec.Cmd
	exited   chan error
	finished bool
	quits    int
	phone    bool
}

func runningAppFixture(t *testing.T, start bool) *runningApp {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("recognising a running app requires macOS process records")
	}
	m := managedTestFixture(t)
	i := m.installer
	managedShortHome(t, i)
	i.path += ":/usr/bin:/bin"
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent-heavy", "AgentQueue"} {
		if err := os.WriteFile(filepath.Join(i.app, "Contents/MacOS", name), raw, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, service := range []string{"queue", "resources", "remote"} {
		m.statuses[service] = "notFound"
	}
	a := &runningApp{managedFixture: m, helper: filepath.Join(i.app, "Contents/MacOS/agent-heavy")}
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "/usr/sbin/lsof" {
			m.calls = append(m.calls, slices.Clone(args))
			output, err := exec.CommandContext(ctx, args[0], args[1:]...).Output()
			return output, err
		}
		if len(args) == 3 && args[1] == "--prepare-app-replacement" {
			a.quits++
			a.stop(t)
		}
		return original(ctx, args)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := i.execute(ctx); err != nil {
		t.Fatal("initial setup", err)
	}
	// Only the real daemon answers from here on.
	i.queueSnapshot = nil
	if start {
		a.start(t)
	}
	t.Cleanup(func() { a.stop(t) })
	return a
}

func (a *runningApp) environment() []string {
	return a.installer.commandEnvironment([]string{a.helper, "status", "--json"})
}

func (a *runningApp) start(t *testing.T) {
	t.Helper()
	i := a.installer
	arguments := []string{fixtureSupervisingApp, a.helper}
	remote := filepath.Join(i.state, "remote")
	if a.phone {
		arguments = append(arguments, filepath.Join(i.app, "Contents/MacOS/agent-remote"), remote)
	}
	a.process = exec.Command(filepath.Join(i.app, "Contents/MacOS/AgentQueue"), arguments...)
	a.process.Env = a.environment()
	if err := a.process.Start(); err != nil {
		t.Fatal(err)
	}
	a.exited = make(chan error, 1)
	go func(process *exec.Cmd, exited chan error) { exited <- process.Wait() }(a.process, a.exited)
	fixtureUntil(t, 30*time.Second, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := i.managedSnapshot(ctx)
		daemon, _ := installInspectLock(i.state, "queue-daemon.lock")
		controller, _ := installInspectLock(i.state, "resource-controller.lock")
		phone, _ := installInspectLock(remote, "supervisor.lock")
		return err == nil && daemon && controller && phone == a.phone
	})
}

func (a *runningApp) stop(t *testing.T) {
	t.Helper()
	if a.process == nil || a.finished {
		return
	}
	a.finished = true
	_ = a.process.Process.Signal(syscall.SIGTERM)
	select {
	case <-a.exited:
	case <-time.After(20 * time.Second):
		_ = a.process.Process.Kill()
		<-a.exited
		t.Error("fixture app did not stop its helpers")
	}
}

func (a *runningApp) requireUntouched(t *testing.T) {
	t.Helper()
	i := a.installer
	if a.quits != 0 || a.finished {
		t.Fatal("the running app was quit")
	}
	managedRequireNoServiceMutation(t, a.calls)
	for _, filename := range []string{filepath.Join(i.home, ".local/bin/agent-heavy"), filepath.Join(i.root, "installation.json")} {
		if _, err := os.Lstat(filename); err != nil {
			t.Fatal("refusal retired", filename, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(i.root, "install-in-progress.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal wrote a pending marker", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := i.managedSnapshot(ctx); err != nil {
		t.Fatal("the running daemon stopped answering", err)
	}
}

func removalContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestManagedRemovalOfRunningAppQuitsItOnceDrained(t *testing.T) {
	a := runningAppFixture(t, true)
	i := a.installer
	finished := exec.Command(a.helper, "run", "--label", "Finished before removal", "--", "/usr/bin/true")
	finished.Env = a.environment()
	if output, err := finished.CombinedOutput(); err != nil {
		t.Fatalf("fixture job: %v: %s", err, output)
	}
	fixtureUntil(t, 10*time.Second, func() bool { return i.requireLegacyCleanup() == nil })
	bundle := installBundleSnapshot(t, i.app)
	managedRemovalTarget(i)
	a.calls = nil
	if err := i.execute(removalContext(t)); err != nil {
		t.Fatal("removal of a running drained app", err)
	}
	if a.quits != 1 || !a.finished {
		t.Fatal("removal did not quit the app once", a.quits)
	}
	quit := slices.IndexFunc(a.calls, func(args []string) bool { return len(args) == 3 && args[1] == "--prepare-app-replacement" })
	unregister := slices.IndexFunc(a.calls, func(args []string) bool { return len(args) == 4 && args[2] == "unregister" && args[3] == "login" })
	checked := slices.IndexFunc(a.calls, func(args []string) bool { return args[0] == "/usr/sbin/lsof" })
	if checked < 0 || quit < checked || unregister < quit {
		t.Fatal("removal must check the holder, then quit, then unregister", checked, quit, unregister)
	}
	for _, name := range []string{"queue-daemon.lock", "resource-controller.lock"} {
		if busy, err := installInspectLock(i.state, name); err != nil || busy {
			t.Fatal("a helper outlived the app", name, busy, err)
		}
	}
	for _, filename := range []string{filepath.Join(i.home, ".local/bin/agent-heavy"), filepath.Join(i.root, "installation.json"), filepath.Join(i.root, "install-in-progress.json")} {
		if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("removal retained", filename, err)
		}
	}
	if _, err := os.Stat(QueueDatabasePath(i.state)); err != nil {
		t.Fatal("removal lost the queue store", err)
	}
	if !reflect.DeepEqual(bundle, installBundleSnapshot(t, i.app)) {
		t.Fatal("removal changed the app")
	}
}

func TestManagedRemovalOfRunningAppStopsItsPhoneService(t *testing.T) {
	a := runningAppFixture(t, false)
	i := a.installer
	raw, err := os.ReadFile(a.helper)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i.app, "Contents/MacOS/agent-remote"), raw, 0700); err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, filepath.Join(i.config, "remote.json"), []byte(`{"enabled":true}`))
	a.phone = true
	a.start(t)
	managedRemovalTarget(i)
	a.calls = nil
	if err := i.execute(removalContext(t)); err != nil {
		t.Fatal("a phone service the app supervises blocked removal", err)
	}
	if a.quits != 1 {
		t.Fatal("removal did not quit the app", a.quits)
	}
	if busy, err := installInspectLock(filepath.Join(i.state, "remote"), "supervisor.lock"); err != nil || busy {
		t.Fatal("the phone service outlived the app", busy, err)
	}
	if settings, err := ReadPrivate(filepath.Join(i.config, "remote.json")); err != nil || settings["enabled"] != true {
		t.Fatal("removal changed the phone access choice", settings, err)
	}
}

func TestManagedRemovalOfRunningAppRefusesActiveJobs(t *testing.T) {
	a := runningAppFixture(t, true)
	i := a.installer
	started, release := filepath.Join(i.home, "started"), filepath.Join(i.home, "release")
	job := exec.Command(a.helper, "run", "--label", "Active during removal", "--", "/bin/sh", "-c", `touch "$1"; while [ ! -f "$2" ]; do sleep .05; done`, "fixture", started, release)
	job.Env = a.environment()
	if err := job.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- job.Wait() }()
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0600)
		<-finished
	})
	fixtureUntil(t, 20*time.Second, func() bool { _, err := os.Stat(started); return err == nil })
	managedRemovalTarget(i)
	a.calls = nil
	err := i.execute(removalContext(t))
	var drain *installDrainPendingError
	if !errors.As(err, &drain) || !strings.Contains(err.Error(), "active or queued jobs") || !strings.Contains(err.Error(), "left running") {
		t.Fatal("active jobs did not refuse removal clearly", err)
	}
	if strings.Contains(err.Error(), "unowned") {
		t.Fatal("a healthy running app was called unowned", err)
	}
	a.requireUntouched(t)
	if _, statErr := os.Stat(release); statErr == nil {
		t.Fatal("fixture job was released early")
	}
	select {
	case err := <-finished:
		finished <- err
		t.Fatal("the active job ended during the refusal", err)
	default:
	}
}

func TestManagedRemovalRefusesWhenDrainCannotBeVerified(t *testing.T) {
	a := runningAppFixture(t, true)
	i := a.installer
	i.queueSnapshot = func(context.Context) (Object, error) { return nil, errors.New("daemon did not answer") }
	managedRemovalTarget(i)
	a.calls = nil
	err := i.execute(removalContext(t))
	if err == nil || !strings.Contains(err.Error(), "Cannot verify") {
		t.Fatal("an unverified queue authorized quitting the app", err)
	}
	i.queueSnapshot = nil
	a.requireUntouched(t)
}

func TestManagedRemovalRefusesUnownedLockHolder(t *testing.T) {
	t.Run("another process", func(t *testing.T) {
		a := runningAppFixture(t, false)
		i := a.installer
		held, err := installLock(i.state, "queue-daemon.lock")
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		managedRemovalTarget(i)
		a.calls = nil
		err = i.execute(removalContext(t))
		if err == nil || !strings.Contains(err.Error(), "unowned queue process") {
			t.Fatal("a foreign lock holder was accepted", err)
		}
		if a.quits != 0 {
			t.Fatal("an unowned holder authorized quitting the app")
		}
		managedRequireNoServiceMutation(t, a.calls)
	})
	t.Run("this app's helper without this app", func(t *testing.T) {
		a := runningAppFixture(t, false)
		i := a.installer
		daemon := exec.Command(a.helper, "daemon")
		daemon.Env = a.environment()
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- daemon.Wait() }()
		t.Cleanup(func() {
			_ = daemon.Process.Signal(syscall.SIGTERM)
			<-exited
		})
		fixtureUntil(t, 20*time.Second, func() bool {
			busy, _ := installInspectLock(i.state, "queue-daemon.lock")
			return busy
		})
		managedRemovalTarget(i)
		a.calls = nil
		if err := i.execute(removalContext(t)); err == nil || !strings.Contains(err.Error(), "unowned queue process") {
			t.Fatal("a helper this app does not supervise was accepted", err)
		}
		if a.quits != 0 {
			t.Fatal("an unowned holder authorized quitting the app")
		}
		managedRequireNoServiceMutation(t, a.calls)
	})
}

func TestManagedSetupAndPreflightNameARunningApp(t *testing.T) {
	a := runningAppFixture(t, true)
	i := a.installer
	a.calls = nil
	report := i.preflight(removalContext(t))
	if !preflightIDs(report)["app-running"] || preflightIDs(report)["managed-setup-blocked"] {
		t.Fatalf("preflight did not name the running app: %+v", report)
	}
	for _, issue := range report.Issues {
		if strings.Contains(issue.Detail, "unowned") {
			t.Fatalf("a healthy running app was called unowned: %+v", issue)
		}
	}
	if err := i.execute(removalContext(t)); !errors.Is(err, errInstallAppRunning) {
		t.Fatal("setup beside a running app did not refuse by name", err)
	}
	a.requireUntouched(t)
}

func TestManagedPreflightStillReportsUnownedLockHolder(t *testing.T) {
	a := runningAppFixture(t, false)
	i := a.installer
	held, err := installLock(i.state, "queue-daemon.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	report := i.preflight(removalContext(t))
	if preflightIDs(report)["app-running"] || !preflightIDs(report)["managed-setup-blocked"] || !strings.Contains(report.Issues[0].Detail, "unowned") {
		t.Fatalf("preflight excused an unowned holder: %+v", report)
	}
}

func TestManagedRemovalWaitsBrieflyForSchedulingLock(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	held, err := installLock(i.state, "scheduling.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	managedRemovalTarget(i)
	time.AfterFunc(300*time.Millisecond, func() { held.Close() })
	if err := i.execute(removalContext(t)); err != nil {
		t.Fatal("a helper's brief hold on scheduling.lock failed removal", err)
	}
}
