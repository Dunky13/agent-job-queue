package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
)

// The real product CLI supplies the daemon, worker and caller. Only
// code-signing/native service registration are injected. This exercises resuming
// a journal an older release left behind: publication and worker outcome writes
// across the replacement of the actual app files in a temporary private home.
// Each scenario names the state setup resumes from, or the failure that leaves
// the following attempt to resume from released, consumed or cancelled custody.
func TestInstallerManagedLiveResumeRetainsRunningAttempts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("managed service handover requires macOS process birth identities")
	}
	helper := buildManagedLiveFixtureCLI(t)
	for _, failure := range []string{"", "queued", "lost commit acknowledgement", "committed", "released", "unregister resources", "unregister login", "register resources"} {
		name := failure
		if name == "" {
			name = "complete"
		}
		t.Run(name, func(t *testing.T) { testInstallerManagedLiveResume(t, helper, failure) })
	}
	for phase, failure := range map[string]string{"released": "register queue", "consumed": "register resources"} {
		t.Run("finish pending from "+phase, func(t *testing.T) { testFinishPendingLiveResume(t, helper, phase, failure) })
	}
}

func buildManagedLiveFixtureCLI(t *testing.T) string {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "agent-heavy")
	// Build once before any scenario changes HOME or starts its 60-second
	// operation budget. Keep race detection while excluding test-only code and
	// debug data from the bundle copied and hashed by every installer phase.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-race", "-p", "2", "-ldflags=-s -w", "-o", helper, "./cmd/agent-heavy")
	command.Dir = filepath.Join("..", "..")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build live installer CLI fixture: %v: %s", err, output)
	}
	return helper
}

func TestInstallerPackagedLiveResume(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("AGENT_QUEUE_TEST_APP_BUNDLE") == "" {
		t.Skip("requires a built macOS app")
	}
	helper := filepath.Join(os.Getenv("AGENT_QUEUE_TEST_APP_BUNDLE"), "Contents/MacOS/agent-heavy")
	for _, failure := range []string{"", "queued", "register resources"} {
		t.Run("packaged-"+failure, func(t *testing.T) { testInstallerManagedLiveResume(t, helper, failure) })
	}
}

// seedOlderReleaseHandover leaves the real daemon and journal as an interrupted
// older release did: prepared against the exact candidate, with nothing replaced.
func seedOlderReleaseHandover(t *testing.T, ctx context.Context, i *installer) *managedLiveUpgrade {
	t.Helper()
	identity, err := i.validatePayload()
	if err != nil {
		t.Fatal(err)
	}
	i.identity = identity
	digest, err := i.liveTargetDigest()
	if err != nil {
		t.Fatal(err)
	}
	value := token()
	prepared, err := i.liveControl(ctx, "prepare-handover", Object{"token": value, "targetSHA256": digest})
	if err != nil {
		t.Fatal("prepare older-release handover", err)
	}
	attempts, err := liveAttempts(prepared)
	if err != nil {
		t.Fatal(err)
	}
	live := &managedLiveUpgrade{SchemaVersion: 1, App: i.app, Target: i.identity, Token: value, TargetSHA256: digest, Attempts: attempts}
	if err := installAtomic(i.liveJournalPath(), append(mustMetadataJSON(t, live), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return live
}

func requireLiveReceiptPhase(t *testing.T, i *installer, phase string) {
	t.Helper()
	receipt, err := readQueueHandoverReceipt(&Gate{State: i.state})
	if err != nil || receipt.Phase != phase {
		t.Fatalf("handover receipt is not %s: %+v: %v", phase, receipt, err)
	}
}

// liveRuntimeFixture installs build 123 with real daemons in a short private home.
// managedShortHome moves the fixture and its bundle under a short private home,
// because Darwin's Unix socket pathname budget is smaller than t.TempDir's path.
func managedShortHome(t *testing.T, i *installer) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "aq-live-")
	if err != nil {
		t.Fatal(err)
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	bundle := i.app
	i.home = filepath.Join(base, "User")
	if err := os.Mkdir(i.home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", i.home)
	i.root = filepath.Join(i.home, ".local/share/agent-heavy")
	i.state = filepath.Join(i.home, ".local/state/agent-heavy")
	i.config = filepath.Join(i.home, ".config/agent-heavy")
	i.launch = filepath.Join(i.home, "Library/LaunchAgents")
	i.app = filepath.Join(i.home, "Applications/Agent Queue.app")
	if err := installCopyTree(bundle, i.app); err != nil {
		t.Fatal(err)
	}
}

func liveRuntimeFixture(t *testing.T, executable string) *managedFixture {
	t.Helper()
	m := managedTestFixture(t)
	i := m.installer
	managedShortHome(t, i)
	i.path += ":/usr/bin:/bin"
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i.app, "Contents/MacOS/agent-heavy"), binary, 0700); err != nil {
		t.Fatal(err)
	}
	managedPackagedRuntimeFixture(t, m)
	setupContext, finishSetup := context.WithTimeout(context.Background(), 60*time.Second)
	defer finishSetup()
	if _, err := runPackagedSetup(setupContext, m.installFixture, func(ctx context.Context) error { return managedOlderRegistered(ctx, m) }); err != nil {
		for _, name := range []string{"daemon.log", "scheduler.log", "readiness.json"} {
			data, _ := os.ReadFile(filepath.Join(i.home, name))
			t.Log(name, string(data))
		}
		t.Fatal("initial private managed installation", err)
	}
	// Fresh installation owns its readiness budget. End it before creating
	// accepted callers so setup cancellation cannot govern the live upgrade.
	finishSetup()
	if !errors.Is(setupContext.Err(), context.Canceled) {
		t.Fatal("initial installation did not complete within its own readiness budget", setupContext.Err())
	}
	return m
}

// callerOutput collects a caller's output while the test may still read it.
type callerOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (o *callerOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.Write(data)
}

func (o *callerOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.String()
}

// liveCallerWait watches one `agent-heavy run` caller until its job is accepted
// and its command starts. Before submitting, the caller identifies the project
// (git subprocesses) and writes fsynced state, and that takes as long as the
// host needs: seconds on a loaded hosted Intel runner. The wait therefore ends
// when the daemon lists the job or the caller exits, and only a total hang
// reaches liveAcceptBudget. The daemon's dispatch and the worker's start are
// bounded local process work and get the shorter liveStartBudget.
type liveCallerWait struct {
	t        *testing.T
	i        *installer
	pid      int
	finished chan error
	output   *callerOutput
	// lifetime, when set, is the context that kills the caller at its deadline.
	lifetime context.Context
}

// liveAcceptBudget bounds only the caller's preparation before the daemon lists
// its job; no test asserts anything about that stage. It replaces an 8 s wait
// that also covered dispatch. Without it a caller that never submits would
// spend the scenario's whole 60 s (3 minutes for finish-pending) context, ten
// hung scenarios would pass the 12 minute `go test` timeout in
// scripts/test-go-runtime.sh, and its panic discards every buffered report.
// 20 s is 4 times the slowest completed preparation measured on a CPU and disk
// stressed hosted Intel runner (5.0 s, a cold first scenario; the rest took 1.1
// s or less) and 2.5 times the 8 s that failed there. That runner took 448 s for
// all ten scenarios, so ten hung preparations add 200 s: 648 s, inside the
// 720 s timeout. A 30 s cap would reach 748 s. See
// docs/handoffs/live-installer-job-start.md.
const liveAcceptBudget = 20 * time.Second

// liveStartBudget bounds the daemon's 100 ms dispatch tick plus one worker
// process start. They took at most 0.6 s on a developer machine and under 2 s
// on a CPU and disk stressed hosted Intel runner.
const liveStartBudget = 8 * time.Second

// accepted waits until the daemon lists exactly jobs jobs. The listing includes
// retained history, and the fixture starts with an empty queue, so the count
// equals the callers started so far.
func (w liveCallerWait) accepted(ctx context.Context, jobs int) {
	w.t.Helper()
	budget, cancel := context.WithTimeout(ctx, liveAcceptBudget)
	defer cancel()
	w.until(budget, fmt.Sprintf("the daemon to accept %d job(s)", jobs), func() bool {
		value, err := w.i.liveControl(budget, "snapshot", nil)
		return err == nil && len(obj(value["tasks"])) == jobs
	})
}

// started waits for the accepted job's command to create path.
func (w liveCallerWait) started(ctx context.Context, path string) {
	w.t.Helper()
	budget, cancel := context.WithTimeout(ctx, liveStartBudget)
	defer cancel()
	w.until(budget, "the accepted job's command to start", func() bool { _, err := os.Stat(path); return err == nil })
}

// until logs how long each stage took, so a late start that still passes leaves
// data (shown with go test -v or on any failure).
func (w liveCallerWait) until(ctx context.Context, what string, ready func() bool) {
	w.t.Helper()
	begin := time.Now()
	for ctx.Err() == nil {
		if ready() {
			w.t.Logf("waited %v for %s", time.Since(begin).Round(time.Millisecond), what)
			return
		}
		select {
		case err := <-w.finished:
			w.finished <- err
			if w.lifetime != nil && w.lifetime.Err() != nil {
				w.t.Fatalf("the scenario deadline expired (%v) and ended the caller (%v) while waiting for %s\n%s", w.lifetime.Err(), err, what, w.describe())
			}
			w.t.Fatalf("the caller exited (%v) while waiting for %s\n%s", err, what, w.describe())
		case <-time.After(40 * time.Millisecond):
		}
	}
	w.t.Fatalf("gave up waiting %v for %s: %v\n%s", time.Since(begin).Round(time.Millisecond), what, ctx.Err(), w.describe())
}

// liveTail returns at most the last 4 KiB of a file, so a runaway log cannot
// flood a failure report.
func liveTail(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err.Error()
	}
	const limit = 4096
	buffer := make([]byte, min(info.Size(), limit))
	count, _ := file.ReadAt(buffer, max(info.Size()-limit, 0))
	return string(buffer[:count])
}

// describe reports the daemon's view of every job, each job log, the caller and
// its subprocesses, and the queue processes, so a hosted-runner failure explains
// itself.
func (w liveCallerWait) describe() string {
	var report strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot, err := w.i.liveControl(ctx, "snapshot", nil)
	fmt.Fprintf(&report, "snapshot error: %v\n", err)
	for id, raw := range obj(snapshot["tasks"]) {
		name, details, statusError := TaskStatus(obj(raw))
		fmt.Fprintf(&report, "job %s: %s %v %v\n", id, name, details, statusError)
	}
	fmt.Fprintf(&report, "queue: %v\n", obj(snapshot["groups"]))
	logs, _ := filepath.Glob(filepath.Join(w.i.state, "logs", "*.log"))
	for _, path := range logs {
		fmt.Fprintf(&report, "log %s: %q\n", filepath.Base(path), liveTail(path))
	}
	select {
	case err := <-w.finished:
		w.finished <- err
		fmt.Fprintf(&report, "caller exited: %v\n", err)
	default:
		report.WriteString("caller still running\n")
	}
	if w.output != nil {
		fmt.Fprintf(&report, "caller output: %q\n", w.output.String())
	}
	for _, path := range []string{filepath.Join(w.i.home, "daemon.log"), filepath.Join(w.i.state, "daemon.log")} {
		fmt.Fprintf(&report, "daemon log: %q\n", liveTail(path))
	}
	// The caller runs in the test's directory, so its git subprocesses do not
	// mention the fixture home. Include the caller's descendants by parentage.
	// Its own limit: a wedged daemon can use up the snapshot's whole context.
	psContext, finishPS := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishPS()
	processes, psError := exec.CommandContext(psContext, "ps", "-axo", "pid,ppid,pgid,etime,stat,command").Output()
	if psError != nil {
		fmt.Fprintf(&report, "process list error: %v\n", psError)
	}
	lines := strings.Split(string(processes), "\n")
	family := map[int]bool{w.pid: true}
	for range 3 {
		for _, line := range lines {
			var pid, parent int
			if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d %d", &pid, &parent); err == nil && family[parent] {
				family[pid] = true
			}
		}
	}
	for _, line := range lines {
		var pid int
		_, _ = fmt.Sscanf(strings.TrimSpace(line), "%d", &pid)
		if strings.Contains(line, w.i.home) || (pid != w.pid && family[pid]) {
			fmt.Fprintln(&report, line)
		}
	}
	return report.String()
}

func testInstallerManagedLiveResume(t *testing.T, executable, failure string) {
	queued := failure == "queued"
	lostCommit := failure == "lost commit acknowledgement"
	committed := failure == "committed"
	released := failure == "released"
	if queued || lostCommit || committed || released {
		failure = ""
	}
	m := liveRuntimeFixture(t, executable)
	i := m.installer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Accepted callers belong to the fixture lifecycle, independently of any
	// installer operation. Their existing cleanup releases and reaps them.
	callersContext, finishCallers := context.WithCancel(context.Background())
	defer finishCallers()
	cli := filepath.Join(i.app, "Contents/MacOS/agent-heavy")
	environment := i.commandEnvironment([]string{cli, "status", "--json"})
	initialControl := i.liveGate(context.Background())
	if _, err := initialControl.QueueControl("limit", Object{"limit": 2}); err != nil {
		t.Fatal(err)
	}
	started, release, runs := filepath.Join(i.home, "started"), filepath.Join(i.home, "release"), filepath.Join(i.home, "attempts")
	var blockerDone chan error
	blockerRelease := filepath.Join(i.home, "blocker-release")
	if queued {
		if _, err := i.liveControl(ctx, "limit", Object{"limit": 1}); err != nil {
			t.Fatal(err)
		}
		blockerStarted := filepath.Join(i.home, "blocker-started")
		blocker := exec.CommandContext(callersContext, cli, "run", "--", "/bin/sh", "-c", `touch "$1"; while [ ! -f "$2" ]; do sleep .05; done`, "fixture", blockerStarted, blockerRelease)
		blocker.Env = environment
		var blockerOutput callerOutput
		blocker.Stdout, blocker.Stderr = &blockerOutput, &blockerOutput
		if err := blocker.Start(); err != nil {
			t.Fatal(err)
		}
		blockerDone = make(chan error, 1)
		go func() { blockerDone <- blocker.Wait() }()
		t.Cleanup(func() {
			_ = os.WriteFile(blockerRelease, nil, 0600)
			if blockerDone != nil {
				<-blockerDone
			}
		})
		blockerWait := liveCallerWait{t: t, i: i, pid: blocker.Process.Pid, finished: blockerDone, output: &blockerOutput}
		blockerWait.accepted(ctx, 1)
		blockerWait.started(ctx, blockerStarted)
	}
	old := exec.CommandContext(callersContext, cli, "run", "--label", "Active across upgrade", "--", "/bin/sh", "-c", `printf 'once\n' >> "$1"; touch "$2"; while [ ! -f "$3" ]; do sleep .05; done; printf 'original outcome\n'; exit 23`, "fixture", runs, started, release)
	old.Env = environment
	var output callerOutput
	old.Stdout, old.Stderr = &output, &output
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- old.Wait() }()
	oldWait := liveCallerWait{t: t, i: i, pid: old.Process.Pid, finished: finished, output: &output}
	joined := false
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0600)
		if joined {
			return
		}
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			_ = old.Process.Kill()
			<-finished
		}
	})
	if queued {
		oldWait.accepted(ctx, 2)
		if _, err := i.liveControl(ctx, "pause", nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blockerRelease, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := <-blockerDone; err != nil {
			t.Fatal("initial queue blocker", err)
		}
		blockerDone = nil
	} else {
		oldWait.accepted(ctx, 1)
		oldWait.started(ctx, started)
	}
	before, err := managedCaptureBundle(i.app)
	if err != nil {
		t.Fatal(err)
	}
	managedOlderPublication(t, m, "124")
	live := seedOlderReleaseHandover(t, ctx, i)
	if committed {
		testResumeCommittedHandover(t, ctx, i, live)
	}
	if released {
		if _, err := i.liveControl(ctx, "commit-handover", Object{"token": live.Token}); err != nil {
			t.Fatal(err)
		}
		fixtureUntil(t, 8*time.Second, func() bool { return i.validateManagedLive(ctx, live) == nil })
	}
	if lostCommit {
		i.queueControl = func(ctx context.Context, operation string, payload Object) (Object, error) {
			response, err := i.liveGate(ctx).QueueControl(operation, payload)
			if operation == "commit-handover" && err == nil {
				return nil, errors.New("simulated lost commit acknowledgement")
			}
			return response, err
		}
	}
	upgrade := func() error {
		// An injected failed installation and the subsequent repair are distinct
		// operations. Busy retries within either one retain its original budget.
		operationContext, finishOperation := context.WithTimeout(context.Background(), 60*time.Second)
		defer finishOperation()
		_, err := runPackagedSetup(operationContext, m.installFixture, i.repairAndExecute)
		return err
	}
	if failure != "" {
		m.nativeFailure = failure
		if err := upgrade(); err == nil {
			t.Fatal("injected native failure unexpectedly completed")
		}
		select {
		case err := <-finished:
			joined = true
			t.Fatalf("interrupted installation lost its accepted caller: %v: %s", err, output.String())
		default:
		}
		m.nativeFailure = ""
		switch failure {
		case "unregister login":
			requireLiveReceiptPhase(t, i, "released")
		case "register resources":
			// The replacement daemon adopts asynchronously after registration.
			fixtureUntil(t, 8*time.Second, func() bool {
				receipt, err := readQueueHandoverReceipt(&Gate{State: i.state})
				return err == nil && receipt.Phase == "consumed"
			})
		case "unregister resources":
			requireLiveReceiptPhase(t, i, "cancelled")
			testCancelledHandoverWaitsForActiveJob(t, ctx, i, upgrade, &output, finished, release, &joined)
			return
		}
	}
	if err := upgrade(); err != nil {
		t.Fatal("resuming the retained handover waited for or interrupted active work", err)
	}
	if _, err := os.Lstat(i.liveJournalPath()); !os.IsNotExist(err) {
		t.Fatal("completed resume retained its journal", err)
	}
	select {
	case err := <-finished:
		joined = true
		t.Fatalf("the accepted caller ended before its job was released: %v: %s", err, output.String())
	default:
	}
	current, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || current.Build == nil || *current.Build != 124 {
		t.Fatal("upgrade did not complete while old job was active", current, err)
	}
	if err := managedRequireBundle(i.runtimeGenerationPath(before), before); err != nil {
		t.Fatal("active worker lost its exact previous signed bundle", err)
	}
	// Completed installer operations must not own later queue callers. Force the
	// earlier budget to expire even on fast machines before verifying work.
	cancel()
	ctx, finishVerification := context.WithTimeout(context.Background(), 60*time.Second)
	defer finishVerification()
	if queued {
		if _, err := os.Stat(started); !os.IsNotExist(err) {
			t.Fatal("handover resumed a deliberately paused queue", err)
		}
		// An RPC budget belongs to one operation, not the entire upgrade.
		// Expire the earlier client deterministically so a fast local run also
		// covers the stale-client failure seen on slower hosted runners.
		initialControl.ControlDeadline = time.Now().Add(-time.Second)
		if _, err := initialControl.QueueControl("snapshot", nil); err == nil || err.Error() != "Queue control deadline expired" {
			t.Fatal("expired initial control unexpectedly remained usable", err)
		}
		if _, err := i.liveControl(ctx, "resume", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := i.liveControl(ctx, "limit", Object{"limit": 2}); err != nil {
			t.Fatal(err)
		}
		oldWait.started(ctx, started)
	}
	newJob := exec.CommandContext(ctx, cli, "run", "--label", "New runtime job", "--", "/bin/sh", "-c", "printf 'new runtime outcome\\n'")
	newJob.Env = environment
	newOutput, err := newJob.CombinedOutput()
	if err != nil || !strings.Contains(string(newOutput), "new runtime outcome") {
		t.Fatal("new runtime could not run work alongside inherited attempt", err, string(newOutput))
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		joined = true
		var exited *exec.ExitError
		if !errors.As(err, &exited) || exited.ExitCode() != 23 || !strings.Contains(output.String(), "original outcome") {
			t.Fatal("original caller lost its exact outcome", err, output.String())
		}
	case <-ctx.Done():
		t.Fatal("original caller did not finish", ctx.Err())
	}
	if data, err := os.ReadFile(runs); err != nil || string(data) != "once\n" {
		t.Fatal("active command was replayed", string(data), err)
	}
	// Reclaim only in a subsequent installer transaction, after terminal outcome
	// and process-death custody are established. Runtime completion keeps code.
	if _, err := os.Stat(i.runtimeGenerationPath(before)); err != nil {
		t.Fatal("runtime completion deleted retained executable code", err)
	}
	if err := i.cleanupManagedRuntimes(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(i.runtimeGenerationPath(before)); !os.IsNotExist(err) {
		t.Fatal("later installer cleanup did not reclaim the completed generation", err)
	}
}

// Hold a daemon connection across commit: the daemon cannot publish released
// custody until that handler returns, which pins the otherwise brief committed
// phase for setup to observe. Its handler deadline is 15 seconds.
func testResumeCommittedHandover(t *testing.T, ctx context.Context, i *installer, live *managedLiveUpgrade) {
	t.Helper()
	held, err := net.Dial("unix", queueSocket(i.liveGate(ctx)))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := i.liveControl(ctx, "commit-handover", Object{"token": live.Token}); err != nil {
		t.Fatal(err)
	}
	requireLiveReceiptPhase(t, i, "committed")
	previous, err := appservices.Load(i.home)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := i.resumeManagedLive(ctx, &managedPlan{previous: previous, loaded: map[string]bool{}})
	if err != nil || resumed == nil || resumed.released {
		t.Fatal("committed journal did not resume", resumed, err)
	}
	// The listener is closed, so a second prepare or commit would fail here.
	if err := i.confirmManagedLive(ctx, resumed); err != nil {
		t.Fatal(err)
	}
	requireLiveReceiptPhase(t, i, "committed")
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := i.commitManagedLive(ctx, resumed); err != nil || !resumed.released {
		t.Fatal("committed journal did not reach released custody", err)
	}
}

// A handover cancelled before commit binds nothing, so setup must not restart
// one: it waits for the job on the still-running daemon, then upgrades drained.
func testCancelledHandoverWaitsForActiveJob(t *testing.T, ctx context.Context, i *installer, upgrade func() error, output *callerOutput, finished chan error, release string, joined *bool) {
	t.Helper()
	var drain *installDrainPendingError
	if _, err := i.planManaged(ctx); !errors.As(err, &drain) {
		t.Fatal("setup did not wait for active work after cancellation", err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		*joined = true
		var exited *exec.ExitError
		if !errors.As(err, &exited) || exited.ExitCode() != 23 {
			t.Fatal("original caller lost its exact outcome", err, output.String())
		}
	case <-ctx.Done():
		t.Fatal("original caller did not finish", ctx.Err())
	}
	if err := upgrade(); err != nil {
		t.Fatal("drained upgrade after cancellation", err)
	}
	current, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || current.Build == nil || *current.Build != 124 {
		t.Fatal("drained upgrade did not complete", current, err)
	}
}

// The shipped route to resume on a newer build. Older build T is interrupted after
// publishing, leaving its marker, retained app and journal. Build N, opened from
// the other Applications folder, finishes T as a non-supervised app bundle using
// N's controller, and relocates once the adopted job has finished.
func testFinishPendingLiveResume(t *testing.T, executable, phase, failure string) {
	m := liveRuntimeFixture(t, executable)
	i := m.installer
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := filepath.Join(i.app, "Contents/MacOS/agent-heavy")
	started, release, runs := filepath.Join(i.home, "started"), filepath.Join(i.home, "release"), filepath.Join(i.home, "attempts")
	caller := exec.CommandContext(ctx, cli, "run", "--label", "Active across recovery", "--", "/bin/sh", "-c", `printf 'once\n' >> "$1"; touch "$2"; while [ ! -f "$3" ]; do sleep .05; done; printf 'original outcome\n'; exit 23`, "fixture", runs, started, release)
	caller.Env = i.commandEnvironment([]string{cli, "status", "--json"})
	var output callerOutput
	caller.Stdout, caller.Stderr = &output, &output
	if err := caller.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- caller.Wait() }()
	callerWait := liveCallerWait{t: t, i: i, pid: caller.Process.Pid, finished: finished, output: &output, lifetime: ctx}
	joined := false
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0600)
		if !joined {
			<-finished
		}
	})
	callerWait.accepted(ctx, 1)
	callerWait.started(ctx, started)
	before, err := managedCaptureBundle(i.app)
	if err != nil {
		t.Fatal(err)
	}
	managedOlderPublication(t, m, "124")
	seedOlderReleaseHandover(t, ctx, i)
	m.nativeFailure = failure
	_, interrupted := runPackagedSetup(ctx, m.installFixture, i.repairAndExecute)
	if interrupted == nil {
		t.Fatal("injected interruption unexpectedly completed", failure)
	}
	m.nativeFailure = ""
	lastPhase := ""
	t.Cleanup(func() {
		if t.Failed() {
			daemon, _ := os.ReadFile(filepath.Join(i.home, "daemon.log"))
			t.Logf("interrupted by: %v; last receipt phase %q; daemon log: %s", interrupted, lastPhase, daemon)
		}
	})
	fixtureUntil(t, 8*time.Second, func() bool {
		receipt, err := readQueueHandoverReceipt(&Gate{State: i.state})
		if err == nil {
			lastPhase = receipt.Phase
		}
		return err == nil && receipt.Phase == phase
	})
	pendingBuild := i.app
	services := map[string]string{"queue": pendingBuild, "resources": pendingBuild, "remote": pendingBuild}
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[0] == "launchctl" && args[1] == "print" {
			label := filepath.Base(args[2])
			service := map[string]string{"io.went.agent-heavy": "queue", "io.went.agent-resources": "resources", "io.went.agent-remote": "remote"}[label]
			if service != "" && m.loaded[label] && m.statuses[service] == "enabled" {
				helper, command := "agent-heavy", "daemon"
				if service == "resources" {
					command = "scheduler"
				} else if service == "remote" {
					helper, command = "agent-remote", "serve"
				}
				return []byte("program = " + filepath.Join(services[service], "Contents/MacOS", helper) + "\narguments = {\n" + helper + "\n" + command + "\n}\n"), nil
			}
		}
		if len(args) == 4 && args[1] == "--service-management" && args[2] == "register" {
			services[args[3]] = filepath.Dir(filepath.Dir(filepath.Dir(args[0])))
		}
		return original(ctx, args)
	}
	moved := filepath.Join(i.home, "Applications/Agent Queue moved.app")
	if err := installCopyTree(i.app, moved); err != nil {
		t.Fatal(err)
	}
	managedBundleBuild(t, i, moved, "125")
	i.app = moved
	report := i.preflight(ctx)
	if len(report.Issues) != 1 || report.Issues[0].ID != "pending-installation" || !slices.ContainsFunc(report.Issues[0].Actions, func(action installRecoveryAction) bool { return action.ID == installFinishPending }) {
		t.Fatalf("automatic setup cannot reach the retained build: %+v", report)
	}
	step, err := installApprovedNext(report)
	if err != nil || step.action != installFinishPending {
		t.Fatal("automatic setup did not select the retained build", step, err)
	}
	m.calls = nil
	i.automaticSetup = true
	if _, err := runPackagedSetup(ctx, m.installFixture, i.finishPendingInstallation); err != nil {
		t.Fatal("the newer build's controller could not finish the retained build", err)
	}
	completed, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || completed.Build == nil || *completed.Build != 124 {
		t.Fatal("the retained build did not complete", completed, err)
	}
	requireLiveJournalGone(t, i)
	// Registering helpers proves the retained app bundle activated non-supervised.
	if !slices.ContainsFunc(m.calls, func(args []string) bool {
		return len(args) == 4 && strings.HasPrefix(args[0], pendingBuild+"/") && args[2] == "register" && args[3] == "resources"
	}) {
		t.Fatal("the retained app bundle did not activate its registered helpers")
	}
	if _, err := os.Stat(i.runtimeGenerationPath(before)); err != nil {
		t.Fatal("resume did not retain the worker's exact previous bundle", err)
	}
	select {
	case err := <-finished:
		joined = true
		t.Fatalf("the caller ended before its job was released: %v: %s", err, output.String())
	default:
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	err = <-finished
	joined = true
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 23 || !strings.Contains(output.String(), "original outcome") {
		t.Fatal("the caller lost its exact outcome", err, output.String())
	}
	if raw, err := os.ReadFile(runs); err != nil || string(raw) != "once\n" {
		t.Fatal("the job was replayed", string(raw), err)
	}
	if _, err := runPackagedSetup(ctx, m.installFixture, i.repairAndExecute); err != nil {
		t.Fatal("the newer build did not relocate after finishing the retained build", err)
	}
	completed, err = installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil || completed.Build == nil || *completed.Build != 125 {
		t.Fatal("the newer build did not complete", completed, err)
	}
	metadata, err := appservices.Load(i.home)
	if err != nil || metadata == nil || metadata.App != moved {
		t.Fatal("the newer location was not published", metadata, err)
	}
}
