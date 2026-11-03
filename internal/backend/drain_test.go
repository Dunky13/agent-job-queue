package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// leftovers lists the private files a submission leaves before it is acknowledged.
func leftovers(g *Gate) []string {
	names := []string{}
	for _, directory := range []string{g.Jobs, filepath.Join(g.State, "workloads"), g.Metadata} {
		entries, _ := os.ReadDir(directory)
		for _, entry := range entries {
			names = append(names, filepath.Join(filepath.Base(directory), entry.Name()))
		}
	}
	return names
}

func readText(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func (f *daemonFixture) fileExists(name string) bool {
	_, err := os.Stat(filepath.Join(f.root, name))
	return err == nil
}

func TestDrainUntilFailsClosed(t *testing.T) {
	g := testGate(t)
	path := drainPath(g)
	if _, draining, err := drainUntil(g); draining || err != nil {
		t.Fatalf("missing file: %v %v", draining, err)
	}
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	mustWrite(t, path, Object{"until": future.Format(time.RFC3339)})
	if until, draining, err := drainUntil(g); !draining || err != nil || !until.Equal(future) {
		t.Fatalf("active drain: %v %v %v", until, draining, err)
	}
	mustWrite(t, path, Object{"until": time.Now().UTC().Add(-time.Second).Format(time.RFC3339)})
	if _, draining, err := drainUntil(g); draining || err != nil {
		t.Fatalf("past expiry must mean not draining: %v %v", draining, err)
	}
	for _, value := range []Object{{}, {"until": "tomorrow"}, {"until": 5}, {"until": future.Format(time.RFC3339), "extra": true}} {
		mustWrite(t, path, value)
		if _, draining, err := drainUntil(g); err == nil || draining {
			t.Errorf("accepted %v", value)
		}
		if drainHolds(g) {
			t.Errorf("an invalid file must not extend a wait: %v", value)
		}
	}
	mustWrite(t, path, Object{"until": future.Format(time.RFC3339)})
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := drainUntil(g); err == nil {
		t.Fatal("accepted a readable drain file")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(g.State, "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := drainUntil(g); err == nil {
		t.Fatal("followed a symlink")
	}
	if _, err := Run(g, []string{"/usr/bin/true"}, ""); err == nil {
		t.Fatal("run went ahead on an unsafe drain file")
	}
}

func TestDrainArguments(t *testing.T) {
	options, err := parseDrainArguments(nil)
	if err != nil || options.duration != 4*time.Hour || options.wait {
		t.Fatalf("defaults: %+v %v", options, err)
	}
	options, err = parseDrainArguments([]string{"--for", "90m", "--wait", "--cancel-queued", "--cancel-running"})
	if err != nil || options.duration != 90*time.Minute || !options.wait || !options.cancelQueued || !options.cancelRunning {
		t.Fatalf("all flags: %+v %v", options, err)
	}
	if options, err = parseDrainArguments([]string{"--for=24h"}); err != nil || options.duration != 24*time.Hour || !options.forGiven {
		t.Fatalf("maximum: %+v %v", options, err)
	}
	if options, err = parseDrainArguments([]string{"--for", "1s"}); err != nil || options.duration != time.Second {
		t.Fatalf("minimum: %+v %v", options, err)
	}
	for _, args := range [][]string{{"--for"}, {"--for", "0"}, {"--for", "-1h"}, {"--for", "24h1s"}, {"--for", "1ms"}, {"--for=999ms"}, {"--for", "soon"}, {"--bogus"}, {"extra"}} {
		if _, err = parseDrainArguments(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestOnDrainArguments(t *testing.T) {
	// Main reads the environment, so the private gate must exist first.
	g := testGate(t)
	for _, test := range []struct {
		input  []string
		lane   string
		policy string
		argv   string
	}{
		{[]string{"--", "sh"}, "", "wait", "sh"},
		{[]string{"--on-drain", "fail", "--", "sh"}, "", "fail", "sh"},
		{[]string{"--on-drain=direct", "--label", "x", "--", "sh"}, "", "direct", "sh"},
		{[]string{"--lane", "qa", "--on-drain", "wait", "--", "sh", "--on-drain", "fail"}, "qa", "wait", "sh --on-drain fail"},
		{[]string{"--on-drain", "fail", "sh", "-c"}, "", "fail", "sh -c"},
	} {
		argv, _, lane, policy, err := runLaneArguments(test.input, true)
		if err != nil || lane != test.lane || policy != test.policy || strings.Join(argv, " ") != test.argv {
			t.Errorf("%v: %v %q %q %v", test.input, argv, lane, policy, err)
		}
	}
	for _, input := range [][]string{{"--on-drain"}, {"--on-drain", "later", "--", "sh"}, {"--on-drain=", "--", "sh"}, {"--on-drain=retry", "--", "sh"}} {
		if _, _, _, _, err := runLaneArguments(input, true); !errors.Is(err, errDrainPolicy) {
			t.Errorf("accepted %v: %v", input, err)
		}
	}
	for _, args := range [][]string{{"run", "--on-drain", "later", "--", "true"}, {"--on-drain", "later", "--", "true"}, {"run", "--on-drain"}} {
		if code := Main(args); code != laneExitUsage {
			t.Errorf("%v: exit %d, want a usage error", args, code)
		}
	}
	// The shorthand without "run" takes the flag too, and a drain needs no daemon.
	mustWrite(t, drainPath(g), Object{"until": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
	if code := Main([]string{"--on-drain", "fail", "--", "/usr/bin/true"}); code != drainExitFail {
		t.Errorf("shorthand with --on-drain: exit %d, want %d", code, drainExitFail)
	}
	if !commandHelp([]string{"run", "--on-drain", "fail", "--help"}) || !commandHelp([]string{"run", "--on-drain=fail", "-h"}) || commandHelp([]string{"run", "--on-drain", "--help", "sh"}) {
		t.Fatal("run --on-drain help detection")
	}
	if !commandHelp([]string{"drain", "--help"}) || !commandHelp([]string{"undrain", "-h"}) {
		t.Fatal("drain help detection")
	}
}

func activeDrain(t *testing.T, g *Gate) time.Time {
	t.Helper()
	until, draining, err := drainUntil(g)
	if err != nil || !draining {
		t.Fatalf("no active drain: %v %v", draining, err)
	}
	return until
}

func TestDrainWithoutDaemonKeepsIntakeClosed(t *testing.T) {
	g := testGate(t)
	if code, err := DrainMain(g, []string{"--for", "1h"}); code != 0 || err != nil {
		t.Fatalf("drain without a daemon: %d %v", code, err)
	}
	hour := activeDrain(t, g)
	if time.Until(hour) > time.Hour+time.Second || time.Until(hour) < 59*time.Minute {
		t.Fatalf("drain expiry: %v", hour)
	}
	// Without --for an active drain keeps its expiry, with or without a daemon flag.
	if code, err := DrainMain(g, nil); code != 0 || err != nil || !activeDrain(t, g).Equal(hour) {
		t.Fatalf("a repeated drain changed the expiry: %d %v", code, err)
	}
	code, err := DrainMain(g, []string{"--cancel-queued"})
	if code != 125 || err == nil || !strings.Contains(err.Error(), "intake stays closed until "+drainClock(hour)) || !strings.Contains(err.Error(), "agent-heavy undrain") || !activeDrain(t, g).Equal(hour) {
		t.Fatalf("a flag that needs the daemon must fail without one, naming the open drain: %d %v", code, err)
	}
	// An explicit --for replaces it, shorter or longer.
	if code, err = DrainMain(g, []string{"--for", "30m"}); code != 0 || err != nil || time.Until(activeDrain(t, g)) > 30*time.Minute+time.Second {
		t.Fatalf("--for must replace the expiry: %d %v", code, err)
	}
	// An active drain with no daemon stays, and callers stay attached, until forced.
	if code, err = UndrainMain(g, nil); code != 1 || err != nil || !drainHolds(g) {
		t.Fatalf("undrain without a daemon must keep the drain: %d %v", code, err)
	}
	if code, err = UndrainMain(g, []string{"--force"}); code != 0 || err != nil {
		t.Fatalf("forced undrain: %d %v", code, err)
	}
	if _, err = os.Lstat(drainPath(g)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drain file remains: %v", err)
	}
	// No active drain: nothing to do, and an expired or invalid file is removed.
	for _, value := range []Object{nil, {"until": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)}, {"until": "garbage"}} {
		if value != nil {
			mustWrite(t, drainPath(g), value)
		}
		if code, err = UndrainMain(g, nil); code != 0 || err != nil {
			t.Fatalf("undrain with no active drain (%v): %d %v", value, code, err)
		}
		if _, err = os.Lstat(drainPath(g)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale drain file remains: %v", err)
		}
	}
	if code, err = UndrainMain(g, []string{"now"}); code != 125 || err == nil {
		t.Fatalf("undrain arguments: %d %v", code, err)
	}
}

func TestUndrainResumesBeforeItRemovesTheDrain(t *testing.T) {
	g := testGate(t)
	mustWrite(t, drainPath(g), Object{"until": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
	resumes, failResume := 0, true
	g.queueControl = func(operation string, payload Object) (Object, error) {
		switch operation {
		case "snapshot":
			return Object{"groups": Object{"default": Object{"status": "Paused", "parallel_tasks": 1}}}, nil
		case "resume":
			resumes++
			if !drainHolds(g) {
				t.Error("the drain was removed before the queue resumed")
			}
			if failResume {
				return nil, errors.New("resume failed")
			}
			return Object{}, nil
		}
		return nil, errors.New("unexpected " + operation)
	}
	if code, err := UndrainMain(g, nil); code != 125 || err == nil || !strings.Contains(err.Error(), "resume failed") || !drainHolds(g) {
		t.Fatalf("a failed resume must keep the drain: %d %v", code, err)
	}
	failResume = false
	if code, err := UndrainMain(g, nil); code != 0 || err != nil || drainHolds(g) || resumes != 2 {
		t.Fatalf("undrain: %d %v resumes=%d", code, err, resumes)
	}
}

func TestRealDaemonDrainHoldsRunUntilUndrain(t *testing.T) {
	f := daemonAcceptance(t)
	f.cli("drain", "--for", "1h").wait(t, 0)
	status := f.cli("status", "--json")
	if output := status.wait(t, 0); !strings.Contains(output, `"drainUntil"`) {
		t.Fatalf("status lacks the drain: %s", output)
	}
	if output := f.cli("status").wait(t, 0); !strings.Contains(output, ", draining until ") || !strings.HasPrefix(output, "Queue running, draining until ") {
		t.Fatalf("status line: %q", output)
	}
	client := f.launch("/bin/sh", "-c", "echo ran >> ran; exit 5")
	client.stderrHas(t, "waiting to submit, no job created yet")
	client.stillRunning(t)
	if ids := f.taskIDs(); len(ids) != 0 {
		t.Fatalf("a job was created during the drain: %v", ids)
	}
	if left := leftovers(f.gate); len(left) != 0 || f.fileExists("ran") {
		t.Fatalf("held caller left %v or ran", left)
	}
	f.cli("undrain").wait(t, 0)
	client.wait(t, 5)
	if readText(t, filepath.Join(f.root, "ran")) != "ran\n" {
		t.Fatal("the command did not run exactly once")
	}
	if !strings.Contains(client.errors.String(), "waiting to submit, no job created yet") || !strings.Contains(client.errors.String(), "agent-heavy undrain") {
		t.Fatalf("no notice naming undrain: %q", client.errors.String())
	}
}

func TestRealDaemonDrainExpiryReopensIntake(t *testing.T) {
	f := daemonAcceptance(t)
	mustWrite(t, drainPath(f.gate), Object{"until": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)})
	f.launch("/bin/sh", "-c", "exit 4").wait(t, 4)
	f.cli("drain", "--for", "2s").wait(t, 0)
	held := f.launch("/bin/sh", "-c", "exit 6")
	held.wait(t, 6)
	if len(f.taskIDs()) != 2 {
		t.Fatal("both callers must have run as jobs")
	}
	mustWrite(t, drainPath(f.gate), Object{"until": "garbage"})
	refused := f.launch("/bin/true")
	if refused.wait(t, 125); !strings.Contains(refused.errors.String(), "drain") {
		t.Fatalf("an invalid drain file must fail closed with its cause: %q", refused.errors.String())
	}
	if _, err := f.gate.QueueControl("submit", Object{"token": token()}); !errors.Is(err, errQueueDraining) || !strings.Contains(err.Error(), "drain file") {
		t.Fatalf("daemon must refuse on an invalid file: %v", err)
	}
}

func TestRealDaemonOnDrainFailLeavesNothing(t *testing.T) {
	f := daemonAcceptance(t)
	f.cli("drain").wait(t, 0)
	client := f.launchOnDrain("fail", "/bin/sh", "-c", "echo forbidden > ran")
	client.wait(t, 75)
	if !strings.Contains(client.errors.String(), "no job was created") || !strings.Contains(client.errors.String(), "until") {
		t.Fatalf("message: %q", client.errors.String())
	}
	if len(f.taskIDs()) != 0 || len(leftovers(f.gate)) != 0 || f.fileExists("ran") {
		t.Fatalf("fail left %v", leftovers(f.gate))
	}
}

func (f *daemonFixture) launchOnDrain(policy string, args ...string) *queueClient {
	f.t.Helper()
	return f.cli(append([]string{"run", "--on-drain", policy, "--"}, args...)...)
}

func TestRealDaemonOnDrainDirectRunsOnceOutsideTheQueue(t *testing.T) {
	f := daemonAcceptance(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.cli("drain").wait(t, 0)
	script := `echo ran >> direct; echo "${AGENT_HEAVY_STATE-unset},$GOMAXPROCS,$GOMEMLIMIT,$GOGC,$AGENT_HEAVY_ACTIVE" > env
echo ready > ready; while [ ! -f go ]; do sleep .05; done
if read -r line; then echo "$line" > stdin; fi
HOME="$2" "$1" run -- /usr/bin/true > nested.out 2>&1; echo $? > nested.code; exit 9`
	// The pipe is empty while the caller checks its input, and holds data afterwards.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	client := f.cliWithStdin(reader, "run", "--on-drain", "direct", "--", "/bin/sh", "-c", script, "fixture", executable, f.root)
	_ = reader.Close()
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("ready") })
	if _, err = writer.WriteString("data\n"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.root, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	client.wait(t, 9)
	if readText(t, filepath.Join(f.root, "direct")) != "ran\n" {
		t.Fatal("the command did not run exactly once")
	}
	environment := strings.Split(strings.TrimSpace(readText(t, filepath.Join(f.root, "env"))), ",")
	if len(environment) != 5 || environment[0] != "unset" || environment[1] != "2" || environment[2] != "512MiB" || environment[3] != "50" || !validToken(environment[4]) {
		t.Fatalf("environment: %v", environment)
	}
	if f.fileExists("stdin") {
		t.Fatalf("direct command read the caller's stdin: %q", readText(t, filepath.Join(f.root, "stdin")))
	}
	if readText(t, filepath.Join(f.root, "nested.code")) != "125\n" || !strings.Contains(readText(t, filepath.Join(f.root, "nested.out")), "Nested") {
		t.Fatal("a nested agent-heavy must be refused inside a direct command")
	}
	if !strings.Contains(client.errors.String(), "outside the queue") {
		t.Fatalf("no notice: %q", client.errors.String())
	}
	if len(f.taskIDs()) != 0 || len(leftovers(f.gate)) != 0 {
		t.Fatalf("direct command appeared in the queue: %v %v", f.taskIDs(), leftovers(f.gate))
	}
	for _, test := range []struct {
		argv []string
		code int
	}{{[]string{"/bin/sh", "-c", "kill -9 $$"}, 137}, {[]string{"/nonexistent/tool"}, 127}, {[]string{"/etc/hosts"}, 126}} {
		f.launchOnDrain("direct", test.argv...).wait(t, test.code)
	}
	lane := f.cli("run", "--on-drain", "direct", "--lane", "qa", "--", "/bin/sh", "-c", "echo forbidden > lane-ran")
	lane.wait(t, laneExitUsage)
	if f.fileExists("lane-ran") || !strings.Contains(lane.errors.String(), "lanes protect exclusive resources") {
		t.Fatalf("direct in a lane: %q", lane.errors.String())
	}
}

func TestRealDaemonOnDrainDirectForwardsSignals(t *testing.T) {
	f := daemonAcceptance(t)
	f.cli("drain").wait(t, 0)
	client := f.launchOnDrain("direct", "/bin/sh", "-c", `trap 'echo term > term; exit 3' TERM; echo ready > ready; while :; do sleep 0.05; done`)
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("ready") })
	if err := client.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	client.wait(t, 3)
	if !f.fileExists("term") {
		t.Fatal("the signal did not reach the command")
	}
}

func TestRealDaemonDrainFinishesQueuedJobsAndWaitsForCleanup(t *testing.T) {
	f := daemonAcceptance(t)
	first := f.launch("/bin/sh", "-c", `while [ ! -f release ]; do sleep .05; done; echo first >> order`)
	f.waitJobs(1)
	second := f.launch("/bin/sh", "-c", "echo second >> order")
	f.waitJobs(2)
	f.cli("drain").wait(t, 0)
	waiter := f.cli("drain", "--wait")
	waiter.stderrHas(t, "waiting for the queue to empty")
	waiter.stillRunning(t)
	if f.fileExists("order") {
		t.Fatal("the queued job started before its turn")
	}
	if err := os.WriteFile(filepath.Join(f.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waiter.wait(t, 0)
	if readText(t, filepath.Join(f.root, "order")) != "first\nsecond\n" {
		t.Fatalf("queued jobs did not both run at the limit: %q", readText(t, filepath.Join(f.root, "order")))
	}
	if entries, _ := os.ReadDir(f.gate.Receipts); len(entries) != 0 {
		t.Fatalf("wait returned before cleanup: %v", entries)
	}
	snapshot, err := f.gate.Snapshot()
	if err != nil || installRequireDrained(obj(snapshot["tasks"])) != nil {
		t.Fatalf("tasks remain: %v", err)
	}
	first.wait(t, 0)
	second.wait(t, 0)
	// With the queue empty, wait returns at once.
	f.cli("drain", "--wait").wait(t, 0)
}

func TestRealDaemonDrainWaitNamesHeldLeaseExpiresAndStopsOnSignal(t *testing.T) {
	f := daemonAcceptance(t)
	f.writePlugin("qa-plugin", `{"schemaVersion":1,"name":"qa-plugin","lanes":[{"name":"qa","label":"QA","concurrency":1,"priority":50,"sharesLimit":false,"reservation":{"memoryMiB":4096,"cpuPercent":100}}]}`)
	f.reloadPlugins()
	owner := startSleeper(t)
	code, lease := f.lane("acquire", "qa", "--owner-pid", strconv.Itoa(owner.Process.Pid))
	if code != 0 || lease["state"] != "held" {
		t.Fatalf("acquire: %d %v", code, lease)
	}
	expiring := f.cli("drain", "--for", "2s", "--wait")
	expiring.wait(t, 1)
	if !strings.Contains(expiring.errors.String(), "intake has reopened") || !strings.Contains(expiring.errors.String(), "held lease "+strconv.Itoa(mustInteger(lease["id"]))+" (lane qa)") {
		t.Fatalf("expiry message: %q", expiring.errors.String())
	}
	waiter := f.cli("drain", "--wait")
	waiter.stderrHas(t, "waiting for the queue to empty")
	if err := waiter.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waiter.wait(t, 128+int(syscall.SIGTERM))
	if !drainHolds(f.gate) || !strings.Contains(waiter.errors.String(), "drain stays in place") {
		t.Fatalf("a signal must leave the drain in place: %q", waiter.errors.String())
	}
}

func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	owner := exec.Command("/bin/sleep", "60")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	return owner
}

func TestRealDaemonDrainCancelQueuedAndRunning(t *testing.T) {
	f := daemonAcceptance(t)
	running := f.launch("/bin/sh", "-c", "trap 'exit 130' INT HUP; echo started > started; sleep 60 & wait")
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("started") })
	queued := f.launch("/bin/sh", "-c", "echo forbidden > queued-ran")
	f.waitJobs(2)
	f.cli("drain", "--cancel-queued").wait(t, 0)
	queued.wait(t, 130)
	running.stillRunning(t)
	f.cli("drain", "--cancel-running").wait(t, 0)
	running.wait(t, 130)
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(f.gate.Slot); return os.IsNotExist(err) })
	if f.fileExists("queued-ran") {
		t.Fatal("a cancelled queued job ran")
	}
}

func TestRealDaemonDrainCancelReleasesHeldLeases(t *testing.T) {
	f := daemonAcceptance(t)
	f.writePlugin("qa-plugin", `{"schemaVersion":1,"name":"qa-plugin","lanes":[{"name":"qa","label":"QA","concurrency":1,"priority":50,"sharesLimit":false,"reservation":{"memoryMiB":4096,"cpuPercent":100}}]}`)
	f.reloadPlugins()
	owner := strconv.Itoa(startSleeper(t).Process.Pid)
	code, held := f.lane("acquire", "qa", "--owner-pid", owner)
	if code != 0 || held["state"] != "held" {
		t.Fatalf("held: %d %v", code, held)
	}
	code, waiting := f.lane("acquire", "qa", "--owner-pid", owner, "--no-wait")
	if code != 0 || waiting["state"] != "queued" {
		t.Fatalf("queued: %d %v", code, waiting)
	}
	heldID, waitingID := strconv.Itoa(mustInteger(held["id"])), strconv.Itoa(mustInteger(waiting["id"]))
	f.cli("drain").wait(t, 0)
	// A drain refuses new leases with the lane exit status for a refusal.
	if code, refused := f.lane("acquire", "qa", "--owner-pid", owner, "--no-wait"); code != laneExitRefused || !strings.Contains(text(refused["error"]), "draining") {
		t.Fatalf("lease during a drain: %d %v", code, refused)
	}
	f.cli("drain", "--cancel-queued").wait(t, 0)
	if code, state := f.lane("status", waitingID); code != 0 || state["state"] != "dropped" {
		t.Fatalf("queued lease: %d %v", code, state)
	}
	if code, state := f.lane("status", heldID); code != 0 || state["state"] != "held" {
		t.Fatalf("--cancel-queued must keep held leases: %d %v", code, state)
	}
	f.cli("drain", "--cancel-running", "--wait").wait(t, 0)
	if code, state := f.lane("status", heldID); code != 0 || state["state"] != "released" {
		t.Fatalf("held lease: %d %v", code, state)
	}
	f.cli("undrain").wait(t, 0)
	if code, again := f.lane("acquire", "qa", "--owner-pid", owner, "--no-wait"); code != 0 || again["state"] != "held" {
		t.Fatalf("lease after undrain: %d %v", code, again)
	}
}

func TestRealDaemonDrainRefusalKeepsTheToken(t *testing.T) {
	f := daemonAcceptance(t)
	f.cli("drain").wait(t, 0)
	value := token()
	for range 2 {
		if _, err := f.gate.QueueControl("submit", Object{"token": value}); !errors.Is(err, errQueueDraining) || !strings.Contains(err.Error(), "until") {
			t.Fatalf("submit during a drain: %v", err)
		}
	}
	if len(f.taskIDs()) != 0 {
		t.Fatal("a refused submission created a job")
	}
	f.cli("undrain").wait(t, 0)
	if _, err := f.gate.QueueControl("submit", Object{"token": value}); err == nil || errors.Is(err, errQueueDraining) {
		t.Fatalf("the token must be usable again, and fail only for its missing payload: %v", err)
	}
}

// raceScenario drives RunInLaneOnDrain against a daemon that changes under it.
// The first submit always opens a drain, as if it began after the caller's check.
type raceScenario struct {
	outcome func(g *Gate, n int) error // the result of submit number n; nil accepts it
	reopen  bool                       // the drain file disappears shortly after it appears
	paused  bool                       // the queue is paused once the drain has gone
	attach  bool                       // an accepted submit while the drain still holds is expected
	noDrain bool                       // no drain ever begins
	submits int
	token   string // the token the caller submitted, which the job must carry
}

func drainRefusal() error { return queueCodedError(errQueueDraining, "Queue is draining until 12:00") }

func (r *raceScenario) gate(t *testing.T) *Gate {
	g := testGate(t)
	if r.outcome == nil {
		r.outcome = func(g *Gate, n int) error {
			if n == 1 {
				return drainRefusal()
			}
			return nil
		}
	}
	g.queueControl = func(operation string, payload Object) (Object, error) {
		switch operation {
		case "snapshot":
			status := "Running"
			if r.paused && !drainHolds(g) && r.submits > 0 {
				status = "Paused"
			}
			return Object{"groups": Object{"default": Object{"status": status, "parallel_tasks": 1}}, "tasks": Object{"3": Object{"token": r.token, "status": Object{"Done": Object{"result": "Success"}}}}}, nil
		case "submit":
			r.submits++
			r.token = text(payload["token"])
			if r.submits == 1 && !r.noDrain {
				mustWrite(t, drainPath(g), Object{"until": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
				if r.reopen {
					time.AfterFunc(900*time.Millisecond, func() { _ = os.Remove(drainPath(g)) })
				}
			}
			if err := r.outcome(g, r.submits); err != nil {
				return nil, err
			}
			if drainHolds(g) && !r.attach {
				t.Error("the caller resubmitted while the drain still held")
			}
			return Object{"id": 3}, nil
		}
		return nil, errors.New("unexpected " + operation)
	}
	return g
}

// lostThrough loses the first n submits to the transport.
func lostThrough(n int, then error) func(g *Gate, i int) error {
	return func(g *Gate, i int) error {
		if i <= n {
			return errQueueTransportUnavailable
		}
		if i == n+1 {
			return then
		}
		return nil
	}
}

func TestRunInLaneOnDrainRaceAppliesThePolicy(t *testing.T) {
	run := func(g *Gate, policy string) (int, error) {
		return RunInLaneOnDrain(g, []string{"/usr/bin/true"}, "", "", policy)
	}
	t.Run("wait", func(t *testing.T) {
		r := &raceScenario{reopen: true}
		g := r.gate(t)
		code, err := run(g, drainWait)
		if code != 0 || err != nil || r.submits != 2 {
			t.Fatalf("code=%d err=%v submits=%d", code, err, r.submits)
		}
	})
	t.Run("fail", func(t *testing.T) {
		r := &raceScenario{}
		g := r.gate(t)
		code, err := run(g, drainFail)
		if code != drainExitFail || err != nil || r.submits != 1 || len(leftovers(g)) != 0 {
			t.Fatalf("code=%d err=%v submits=%d left=%v", code, err, r.submits, leftovers(g))
		}
	})
	t.Run("direct", func(t *testing.T) {
		r := &raceScenario{}
		g := r.gate(t)
		root := t.TempDir()
		script := `ls "$1" > "$3/during-jobs"; ls "$2" > "$3/during-workloads"; echo run >> "$3/count"; exit 4`
		code, err := RunInLaneOnDrain(g, []string{"/bin/sh", "-c", script, "fixture", g.Jobs, filepath.Join(g.State, "workloads"), root}, "", "", drainDirect)
		if code != 4 || err != nil || r.submits != 1 || readText(t, filepath.Join(root, "count")) != "run\n" || len(leftovers(g)) != 0 {
			t.Fatalf("code=%d err=%v submits=%d left=%v", code, err, r.submits, leftovers(g))
		}
		if readText(t, filepath.Join(root, "during-jobs"))+readText(t, filepath.Join(root, "during-workloads")) != "" {
			t.Fatal("the submission files were still present while the command ran")
		}
	})
	t.Run("a paused queue after the wait is a definite refusal", func(t *testing.T) {
		r := &raceScenario{reopen: true, paused: true}
		g := r.gate(t)
		code, err := run(g, drainWait)
		if code != 125 || err == nil || !strings.Contains(err.Error(), "must be Running") || r.submits != 1 || len(leftovers(g)) != 0 {
			t.Fatalf("code=%d err=%v submits=%d left=%v", code, err, r.submits, leftovers(g))
		}
	})
	t.Run("an invalid drain file refused by the daemon fails closed and cleans up", func(t *testing.T) {
		r := &raceScenario{}
		g := r.gate(t)
		r.outcome = func(g *Gate, n int) error {
			mustWrite(t, drainPath(g), Object{"until": "garbage"})
			return queueCodedError(errQueueDraining, "Queue is draining; no new work is accepted: Invalid drain file")
		}
		code, err := run(g, drainWait)
		if code != 125 || err == nil || !strings.Contains(err.Error(), "Invalid drain file") || r.submits != 1 || len(leftovers(g)) != 0 {
			t.Fatalf("code=%d err=%v submits=%d left=%v", code, err, r.submits, leftovers(g))
		}
	})
	t.Run("a transport loss during a drain keeps retrying and attaches when the job was committed", func(t *testing.T) {
		r := &raceScenario{attach: true}
		g := r.gate(t)
		r.outcome = lostThrough(3, nil)
		code, err := run(g, drainWait)
		if code != 0 || err != nil || r.submits != 4 {
			t.Fatalf("code=%d err=%v submits=%d", code, err, r.submits)
		}
	})
	t.Run("a draining refusal after a loss means nothing was committed and the hold continues", func(t *testing.T) {
		r := &raceScenario{}
		g := r.gate(t)
		r.outcome = lostThrough(3, drainRefusal())
		stderr := captureStderr(t)
		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			code, err := run(g, drainWait)
			done <- result{code, err}
		}()
		// The ordinary notice appears only once the daemon answered with a refusal.
		waitForText(t, stderr, "no job created yet")
		if err := os.Remove(drainPath(g)); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			if got.code != 0 || got.err != nil || r.submits != 5 {
				t.Fatalf("code=%d err=%v submits=%d", got.code, got.err, r.submits)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the caller did not continue after the drain ended")
		}
		if !strings.Contains(stderr.String(), "outcome unknown") {
			t.Fatalf("the unknown outcome was never reported: %q", stderr.String())
		}
	})
	t.Run("an answer after a lost response never discards the files, drain or not", func(t *testing.T) {
		for _, noDrain := range []bool{true, false} {
			r := &raceScenario{noDrain: noDrain}
			g := r.gate(t)
			r.outcome = func(g *Gate, n int) error {
				if n == 1 {
					return errQueueTransportUnavailable
				}
				return &queueAnsweredError{"database is locked"}
			}
			if noDrain {
				code, err := run(g, drainWait)
				if code != 125 || err == nil || !strings.Contains(err.Error(), "database is locked") || !strings.Contains(err.Error(), "a job may already exist, check agent-heavy status") || len(leftovers(g)) == 0 {
					t.Fatalf("code=%d err=%v left=%v", code, err, leftovers(g))
				}
				continue
			}
			// With a drain the caller retries once the daemon answers, and still keeps the files.
			r.outcome = func(g *Gate, n int) error {
				if n <= 3 {
					return errQueueTransportUnavailable
				}
				return &queueAnsweredError{"database is locked"}
			}
			code, err := run(g, drainWait)
			if code != 125 || err == nil || !strings.Contains(err.Error(), "database is locked") || len(leftovers(g)) == 0 {
				t.Fatalf("drain: code=%d err=%v left=%v", code, err, leftovers(g))
			}
		}
	})
	t.Run("a coordination lock failure after a lost response says a job may exist and keeps the files", func(t *testing.T) {
		r := &raceScenario{}
		g := r.gate(t)
		r.outcome = func(g *Gate, n int) error { return errQueueTransportUnavailable }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g.controlContext = ctx
		stderr := captureStderr(t)
		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			code, err := run(g, drainWait)
			done <- result{code, err}
		}()
		waitForText(t, stderr, "submission outcome unknown")
		cancel()
		select {
		case got := <-done:
			if got.code != 125 || got.err == nil || !strings.Contains(got.err.Error(), "a job may already exist, check agent-heavy status") || len(leftovers(g)) == 0 {
				t.Fatalf("code=%d err=%v left=%v", got.code, got.err, leftovers(g))
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the caller did not end")
		}
	})
	t.Run("a refusal by the daemon leaves nothing behind but a lost response does", func(t *testing.T) {
		r := &raceScenario{}
		g := r.gate(t)
		r.outcome = func(g *Gate, n int) error {
			return &queueAnsweredError{"Dedicated default queue must be Running; no command was submitted"}
		}
		if code, err := run(g, drainWait); code != 125 || err == nil || len(leftovers(g)) != 0 {
			t.Fatalf("answered: code=%d err=%v left=%v", code, err, leftovers(g))
		}
	})
	t.Run("fail and direct never act on an uncertain outcome", func(t *testing.T) {
		for _, policy := range []string{drainFail, drainDirect} {
			r := &raceScenario{}
			g := r.gate(t)
			r.outcome = lostThrough(3, nil)
			root := t.TempDir()
			code, err := RunInLaneOnDrain(g, []string{"/bin/sh", "-c", "echo forbidden > " + filepath.Join(root, "ran")}, "", "", policy)
			if code != 125 || !errors.Is(err, errQueueTransportUnavailable) || r.submits != 3 || len(leftovers(g)) == 0 {
				t.Fatalf("%s: code=%d err=%v submits=%d left=%v", policy, code, err, r.submits, leftovers(g))
			}
			if _, statErr := os.Stat(filepath.Join(root, "ran")); statErr == nil {
				t.Fatalf("%s ran a command after an uncertain submit", policy)
			}
		}
	})
	t.Run("a signal during preparation creates no job", func(t *testing.T) {
		r := &raceScenario{}
		g := r.gate(t)
		inner := g.queueControl
		probe := make(chan os.Signal, 1)
		signal.Notify(probe, syscall.SIGTERM)
		defer signal.Stop(probe)
		g.queueControl = func(operation string, payload Object) (Object, error) {
			if operation == "snapshot" {
				// RunInLane has registered its handler; the signal arrives while it prepares.
				if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
					t.Error(err)
				}
				<-probe
			}
			return inner(operation, payload)
		}
		code, err := run(g, drainWait)
		if code != 128+int(syscall.SIGTERM) || err != nil || r.submits != 0 || len(leftovers(g)) != 0 {
			t.Fatalf("code=%d err=%v submits=%d left=%v", code, err, r.submits, leftovers(g))
		}
	})
}

func TestRealDaemonDrainKeepsCallerAttachedAcrossDaemonRestart(t *testing.T) {
	f := daemonAcceptance(t)
	client := f.launch("/bin/sh", "-c", `printf x >> "$1"; while [ ! -f "$2" ]; do sleep .05; done; exit 23`, "fixture", filepath.Join(f.root, "attempts"), filepath.Join(f.root, "release"))
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("attempts") })
	id := f.taskIDs()[0]
	f.cli("drain").wait(t, 0)
	f.stopDaemon()
	client.stderrHas(t, "(drain active); still waiting for job")
	if err := os.WriteFile(filepath.Join(f.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureUntil(t, 8*time.Second, func() bool {
		s, err := openQueueStore(f.gate.State, false)
		if err != nil {
			return false
		}
		defer s.close()
		j, err := s.job(id)
		return err == nil && j.phase == "outcome" && j.outcome.Valid && j.outcome.Int64 == 23
	})
	client.stillRunning(t)
	queueRestartFixture(t, f)
	client.wait(t, 23)
	if readText(t, filepath.Join(f.root, "attempts")) != "x" {
		t.Fatalf("the command did not run exactly once: %q", readText(t, filepath.Join(f.root, "attempts")))
	}
	snapshot, err := f.gate.Snapshot()
	if err != nil || obj(obj(snapshot["groups"])["default"])["status"] != "Paused" {
		t.Fatalf("restart must pause: %v", err)
	}
}

func TestRealDaemonDrainQueuedCallerSurvivesRestartAndRunsAfterUndrain(t *testing.T) {
	f := daemonAcceptance(t)
	first := f.launch("/bin/sh", "-c", `printf x >> first-runs; while [ ! -f release ]; do sleep .05; done`)
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("first-runs") })
	second := f.launch("/bin/sh", "-c", "printf y >> second-runs; exit 7")
	f.waitJobs(2)
	f.cli("drain").wait(t, 0)
	f.stopDaemon()
	second.stderrHas(t, "(drain active); still waiting for job")
	if err := os.WriteFile(filepath.Join(f.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	queueRestartFixture(t, f)
	first.wait(t, 0)
	snapshot, err := f.gate.Snapshot()
	if err != nil || obj(obj(snapshot["groups"])["default"])["status"] != "Paused" || f.fileExists("second-runs") {
		t.Fatalf("the restarted queue must be paused with the queued job not started: %v", err)
	}
	second.stillRunning(t)
	undrain := f.cli("undrain")
	undrain.wait(t, 0)
	if !strings.Contains(undrain.errors.String(), "was paused and is resumed") {
		t.Fatalf("undrain must say it ended a pause: %q", undrain.errors.String())
	}
	second.wait(t, 7)
	if readText(t, filepath.Join(f.root, "second-runs")) != "y" || readText(t, filepath.Join(f.root, "first-runs")) != "x" {
		t.Fatal("a command ran more than once")
	}
}

func TestRealDaemonDrainKeepsAWorkerWaitingForTheSlotAcrossRestart(t *testing.T) {
	f := daemonAcceptance(t)
	if err := os.Mkdir(f.gate.Slot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(f.gate.Slot, "owner")
	if err := os.WriteFile(owner, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := f.launch("/bin/sh", "-c", "printf x >> ran; exit 8")
	f.waitJobs(1)
	id := f.taskIDs()[0]
	fixtureUntil(t, 8*time.Second, func() bool {
		receipt, err := f.gate.Receipt(id)
		return err == nil && receipt != nil && receipt["pgid"] == nil
	})
	f.cli("drain").wait(t, 0)
	f.stopDaemon()
	client.stderrHas(t, "(drain active); still waiting for job")
	if f.fileExists("ran") {
		t.Fatal("the command ran while the external owner held the slot")
	}
	queueRestartFixture(t, f)
	if err := os.Remove(owner); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.gate.Slot); err != nil {
		t.Fatal(err)
	}
	client.wait(t, 8)
	if readText(t, filepath.Join(f.root, "ran")) != "x" {
		t.Fatal("the command did not run exactly once")
	}
}

func (f *daemonFixture) stopDaemon() {
	f.t.Helper()
	if err := f.daemon.Process.Signal(syscall.SIGTERM); err != nil {
		f.t.Fatal(err)
	}
	if err := f.daemon.Wait(); err != nil {
		f.t.Fatalf("daemon stop: %v", err)
	}
}

func (f *daemonFixture) queueStatus() any {
	f.t.Helper()
	snapshot, err := f.gate.Snapshot()
	if err != nil {
		f.t.Fatal(err)
	}
	return obj(obj(snapshot["groups"])["default"])["status"]
}

func TestRealDaemonUndrainWithoutADrainLeavesAPauseAlone(t *testing.T) {
	f := daemonAcceptance(t)
	f.cli("pause").wait(t, 0)
	undrain := f.cli("undrain")
	undrain.wait(t, 0)
	if f.queueStatus() != "Paused" || !strings.Contains(undrain.errors.String(), "no drain is active") {
		t.Fatalf("undrain without a drain must not resume: %v %q", f.queueStatus(), undrain.errors.String())
	}
	mustWrite(t, drainPath(f.gate), Object{"until": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)})
	f.cli("undrain").wait(t, 0)
	if _, err := os.Lstat(drainPath(f.gate)); !errors.Is(err, os.ErrNotExist) || f.queueStatus() != "Paused" {
		t.Fatalf("an expired drain file must be removed without resuming: %v %v", err, f.queueStatus())
	}
	f.cli("drain").wait(t, 0)
	resumed := f.cli("undrain")
	resumed.wait(t, 0)
	if f.queueStatus() != "Running" || drainHolds(f.gate) || !strings.Contains(resumed.errors.String(), "was paused and is resumed") {
		t.Fatalf("undrain with a drain must resume and remove it: %v %q", f.queueStatus(), resumed.errors.String())
	}
}

func TestRealDaemonUndrainRefusesWhileTheDaemonIsAway(t *testing.T) {
	f := daemonAcceptance(t)
	f.cli("drain").wait(t, 0)
	f.stopDaemon()
	refused := f.cli("undrain")
	refused.wait(t, 1)
	if !drainHolds(f.gate) || !strings.Contains(refused.errors.String(), "--force") {
		t.Fatalf("undrain must keep the drain and say how to force it: %q", refused.errors.String())
	}
	f.cli("undrain", "--force").wait(t, 0)
	if drainHolds(f.gate) {
		t.Fatal("--force kept the drain")
	}
}

func TestRealDaemonDrainSignalEndsAWaitingCallerWithNothingBehind(t *testing.T) {
	f := daemonAcceptance(t)
	f.cli("drain").wait(t, 0)
	client := f.launch("/bin/sh", "-c", "echo forbidden > ran")
	client.stderrHas(t, "waiting to submit, no job created yet")
	if err := client.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	client.wait(t, 128+int(syscall.SIGTERM))
	if len(f.taskIDs()) != 0 || len(leftovers(f.gate)) != 0 || f.fileExists("ran") {
		t.Fatalf("an interrupted wait left %v %v", f.taskIDs(), leftovers(f.gate))
	}
}

func TestRealDaemonDrainDetachesAnInterruptedCallerWithoutCancelling(t *testing.T) {
	f := daemonAcceptance(t)
	client := f.launch("/bin/sh", "-c", `printf x >> "$1"; while [ ! -f "$2" ]; do sleep .05; done; exit 23`, "fixture", filepath.Join(f.root, "attempts"), filepath.Join(f.root, "release"))
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("attempts") })
	id := f.taskIDs()[0]
	f.cli("drain").wait(t, 0)
	f.stopDaemon()
	client.stderrHas(t, "(drain active); still waiting for job")
	if err := client.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	client.wait(t, 128+int(syscall.SIGTERM))
	if !strings.Contains(client.errors.String(), "detaching; job "+strconv.Itoa(id)+" stays in the queue, use agent-heavy cancel "+strconv.Itoa(id)+" (a running job keeps running)") {
		t.Fatalf("detach message: %q", client.errors.String())
	}
	if err := os.WriteFile(filepath.Join(f.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	queueRestartFixture(t, f)
	fixtureUntil(t, 10*time.Second, func() bool {
		task, err := f.gate.Task(id)
		if err != nil {
			return false
		}
		name, details, err := TaskStatus(task)
		if name != "Done" || err != nil {
			return false
		}
		code, err := ResultCode(details["result"])
		return err == nil && code == 23
	})
	if readText(t, filepath.Join(f.root, "attempts")) != "x" {
		t.Fatal("the detached job did not run exactly once")
	}
}

func TestWaitCleanupInADrainGap(t *testing.T) {
	setup := func(t *testing.T, drain bool) (*Gate, string, *atomic.Int32, *atomic.Bool) {
		g := testGate(t)
		value := token()
		mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": value})
		if drain {
			mustWrite(t, drainPath(g), Object{"until": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
		}
		shortened := queueCleanupWindow
		queueCleanupWindow = 200 * time.Millisecond
		t.Cleanup(func() { queueCleanupWindow = shortened })
		probes, up := &atomic.Int32{}, &atomic.Bool{}
		g.queueControl = func(operation string, payload Object) (Object, error) {
			probes.Add(1)
			if up.Load() {
				return Object{"tasks": Object{}}, nil
			}
			return nil, errQueueTransportUnavailable
		}
		return g, value, probes, up
	}
	run := func(g *Gate, value string, signals chan os.Signal) func() error {
		done := make(chan error, 1)
		go func() { done <- waitCleanupWithToken(g, 7, value, signals) }()
		return func() error {
			select {
			case err := <-done:
				return err
			case <-time.After(10 * time.Second):
				t.Fatal("the cleanup wait did not end")
				return nil
			}
		}
	}
	// A signal ends the wait with the unconfirmed-cleanup note, which the follow
	// loop reports beside the job's own exit status.
	detached := func(t *testing.T, err error) {
		t.Helper()
		var unconfirmed *queueCleanupUnconfirmed
		if !errors.As(err, &unconfirmed) || !strings.Contains(err.Error(), "Detaching during a drain") {
			t.Fatalf("want a detach note, got %v", err)
		}
	}
	t.Run("a signal detaches in the gap", func(t *testing.T) {
		g, value, probes, _ := setup(t, true)
		signals := make(chan os.Signal, 1)
		done := run(g, value, signals)
		fixtureUntil(t, 10*time.Second, func() bool { return probes.Load() >= 2 })
		signals <- syscall.SIGTERM
		detached(t, done())
	})
	t.Run("a signal detaches during a drain before the cleanup deadline", func(t *testing.T) {
		g, value, probes, _ := setup(t, true)
		queueCleanupWindow = time.Minute
		signals := make(chan os.Signal, 1)
		signals <- syscall.SIGTERM
		detached(t, run(g, value, signals)())
		if probes.Load() != 0 {
			t.Fatal("the signal waited for the deadline")
		}
	})
	t.Run("a returning daemon ends the wait", func(t *testing.T) {
		g, value, probes, up := setup(t, true)
		done := run(g, value, make(chan os.Signal, 1))
		fixtureUntil(t, 10*time.Second, func() bool { return probes.Load() >= 2 })
		up.Store(true)
		if err := os.Remove(g.ReceiptPath(7)); err != nil {
			t.Fatal(err)
		}
		if err := done(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("without a drain the wait still fails and a signal is not consumed", func(t *testing.T) {
		g, value, _, _ := setup(t, false)
		signals := make(chan os.Signal, 1)
		signals <- syscall.SIGTERM
		var unconfirmed *queueCleanupUnconfirmed
		if err := run(g, value, signals)(); !errors.As(err, &unconfirmed) || strings.Contains(err.Error(), "Detaching") || len(signals) != 1 {
			t.Fatalf("err=%v pending signals=%d", err, len(signals))
		}
	})
}

func TestRealDaemonOnDrainDirectOwnsItsSessionAndProcessGroup(t *testing.T) {
	f := daemonAcceptance(t)
	f.cli("drain").wait(t, 0)
	client := f.launchOnDrain("direct", "/bin/sh", "-c", `sleep 60 & echo $! > child; echo $$ > shell; ps -o pgid= -p $$ > pgid; ps -o stat= -p $$ > stat; echo ready > ready; wait`)
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("ready") })
	number := func(name string) int {
		value, err := strconv.Atoi(strings.TrimSpace(readText(t, filepath.Join(f.root, name))))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	child, shell := number("child"), number("shell")
	if number("pgid") != shell || shell == client.command.Process.Pid {
		t.Fatalf("the direct command must lead its own process group: pgid=%d shell=%d caller=%d", number("pgid"), shell, client.command.Process.Pid)
	}
	// A new session has no controlling terminal, so a background tty access cannot
	// stop the command; ps marks the session leader with "s". There is no pty
	// helper in the repository, so this is the observable stand-in.
	if !strings.Contains(readText(t, filepath.Join(f.root, "stat")), "s") {
		t.Fatalf("the direct command must lead its own session: stat=%q", readText(t, filepath.Join(f.root, "stat")))
	}
	if err := client.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The caller only returns once the command's descendants released its stdout.
	client.wait(t, 128+int(syscall.SIGTERM))
	fixtureUntil(t, 8*time.Second, func() bool { return errors.Is(syscall.Kill(child, 0), syscall.ESRCH) })
}

func TestRealDaemonDrainWaitSaysWhenTheQueueIsPaused(t *testing.T) {
	f := daemonAcceptance(t)
	job := f.launch("/bin/sh", "-c", "echo started > started; while [ ! -f release ]; do sleep .05; done")
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("started") })
	f.cli("pause").wait(t, 0)
	waiter := f.cli("drain", "--wait")
	waiter.stderrHas(t, "the queue is paused, so queued jobs will not start until agent-heavy resume or agent-heavy undrain")
	if err := os.WriteFile(filepath.Join(f.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waiter.wait(t, 0)
	job.wait(t, 0)
}

func TestRealDaemonDrainWaitLostDaemonNamesTheOpenDrain(t *testing.T) {
	f := daemonAcceptance(t)
	job := f.launch("/bin/sh", "-c", "while [ ! -f release ]; do sleep .05; done")
	f.waitJobs(1)
	waiter := f.cli("drain", "--wait")
	waiter.stderrHas(t, "waiting for the queue to empty")
	f.stopDaemon()
	waiter.wait(t, 125)
	if !strings.Contains(waiter.errors.String(), "intake stays closed until") || !strings.Contains(waiter.errors.String(), "agent-heavy undrain") {
		t.Fatalf("message: %q", waiter.errors.String())
	}
	if err := os.WriteFile(filepath.Join(f.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	queueRestartFixture(t, f)
	job.wait(t, 0)
}

// captureStderr redirects os.Stderr into a buffer a test can read while the
// code under test is still running.
func captureStderr(t *testing.T) *lockedBuffer {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	buffer := &lockedBuffer{}
	original := os.Stderr
	os.Stderr = writer
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(buffer, reader); close(copied) }()
	t.Cleanup(func() {
		os.Stderr = original
		_ = writer.Close()
		<-copied
		_ = reader.Close()
	})
	return buffer
}

func waitForText(t *testing.T, buffer *lockedBuffer, text string) {
	t.Helper()
	fixtureUntil(t, 10*time.Second, func() bool { return strings.Contains(buffer.String(), text) })
}

func TestDrainExpiryRoundsUp(t *testing.T) {
	for _, nanos := range []int{0, 1, 400_000_000, 999_999_999} {
		now := time.Date(2026, 11, 3, 10, 0, 0, nanos, time.UTC)
		for _, duration := range []time.Duration{time.Second, 1500 * time.Millisecond, time.Hour} {
			until := drainExpiry(now, duration)
			if until.Before(now.Add(duration)) || until.Sub(now) >= duration+time.Second || until.Nanosecond() != 0 {
				t.Errorf("now=%v for %v: until=%v", now, duration, until)
			}
		}
	}
}

func TestCancelQueuedOperationOnlyTouchesWhatHasNotStarted(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1}`)})
	insert := func(id int, phase, attempt, state string) {
		value := fmt.Sprintf("%032x", id+1)
		task := Object{"token": value, "priority": 0, "created_at": queueNow(), "status": Object{state: Object{}}, "logPath": filepath.Join(d.g.State, "logs", strconv.Itoa(id)+".log")}
		data, err := queueJSON(task)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.store.db.Exec("INSERT INTO queue_jobs(id,token,task,attempt,phase) VALUES(?,?,?,?,?)", id, value, data, attempt, phase); err != nil {
			t.Fatal(err)
		}
	}
	held := acquireTestLease(t, d, "qa", nil)
	queuedLease := acquireTestLease(t, d, "qa", nil)
	// Grant the first lease before any command exists for the tick to start.
	if err := d.tick(); err != nil {
		t.Fatal(err)
	}
	if state := leaseState(t, d, held)["state"]; state != "held" {
		t.Fatalf("lease fixtures: %v", state)
	}
	insert(10, "running", strings.Repeat("a", 32), "Running")
	insert(11, "starting", strings.Repeat("b", 32), "Running")
	insert(12, "queued", "", "Queued")
	insert(13, "queued", "", "Stashed")
	result, err := d.control("cancel-queued", nil)
	if err != nil || mustInteger(result["cancelled"]) != 3 {
		t.Fatalf("cancel-queued: %v %v", result, err)
	}
	for _, id := range []int{12, 13} {
		j, err := d.store.job(id)
		if err != nil || j.phase != "outcome" || !j.outcome.Valid || j.outcome.Int64 != 130 || !boolean(j.task["cancelled"]) {
			t.Fatalf("queued job %d: %+v %v", id, j, err)
		}
	}
	for id, phase := range map[int]string{10: "running", 11: "starting"} {
		j, err := d.store.job(id)
		if err != nil || j.phase != phase || boolean(j.task["cancelled"]) {
			t.Fatalf("started job %d was touched: %+v %v", id, j, err)
		}
	}
	if state := leaseState(t, d, queuedLease)["state"]; state != "dropped" {
		t.Fatalf("queued lease: %v", state)
	}
	if state := leaseState(t, d, held)["state"]; state != "held" {
		t.Fatalf("held lease: %v", state)
	}
	if again, err := d.control("cancel-queued", nil); err != nil || mustInteger(again["cancelled"]) != 0 {
		t.Fatalf("second pass: %v %v", again, err)
	}
}

func TestDrainCancelsQueuedWorkInOneDaemonPassBeforeRunningWork(t *testing.T) {
	g := testGate(t)
	var operations []string
	running := true
	g.queueControl = func(operation string, payload Object) (Object, error) {
		operations = append(operations, operation)
		switch operation {
		case "snapshot":
			state := Object{"Running": Object{}}
			if !running {
				state = Object{"Done": Object{"result": "Success"}}
			}
			return Object{"tasks": Object{"1": Object{"status": Object{"Queued": Object{}}}, "2": Object{"status": Object{"Queued": Object{}}}, "3": Object{"status": state}}}, nil
		case "cancel-queued":
			return Object{"cancelled": 2}, nil
		case "cancel":
			if mustInteger(payload["id"]) != 3 {
				t.Errorf("a queued job was cancelled one by one: %v", payload)
			}
			running = false
			return Object{}, nil
		}
		return nil, errors.New("unexpected " + operation)
	}
	stderr := captureStderr(t)
	if code, err := DrainMain(g, []string{"--cancel-queued", "--cancel-running"}); code != 0 || err != nil {
		t.Fatalf("drain: %d %v", code, err)
	}
	var cancels []string
	for _, operation := range operations {
		if operation == "cancel-queued" || operation == "cancel" {
			cancels = append(cancels, operation)
		}
	}
	if !slices.Equal(cancels, []string{"cancel-queued", "cancel"}) {
		t.Fatalf("operations: %v", operations)
	}
	waitForText(t, stderr, "cancelled 2 queued and 1 running or held item(s)")
}

func TestRealDaemonDrainCancelQueuedAndRunningTogether(t *testing.T) {
	f := daemonAcceptance(t)
	running := f.launch("/bin/sh", "-c", "trap 'exit 130' INT HUP; echo started > started; sleep 60 & wait")
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("started") })
	first := f.launch("/bin/sh", "-c", "echo forbidden > queued-ran-1")
	second := f.launch("/bin/sh", "-c", "echo forbidden > queued-ran-2")
	f.waitJobs(3)
	drain := f.cli("drain", "--cancel-queued", "--cancel-running")
	drain.wait(t, 0)
	if !strings.Contains(drain.errors.String(), "cancelled 2 queued and 1 running or held item(s)") {
		t.Fatalf("counts: %q", drain.errors.String())
	}
	for _, client := range []*queueClient{running, first, second} {
		client.wait(t, 130)
	}
	if f.fileExists("queued-ran-1") || f.fileExists("queued-ran-2") {
		t.Fatal("a queued job started and ran while the running one was being cancelled")
	}
}

func TestRealDaemonDrainWaitHoldsForACleanupReceipt(t *testing.T) {
	f := daemonAcceptance(t)
	mustWrite(t, f.gate.ReceiptPath(999), Object{"task_id": 999, "token": token()})
	waiter := f.cli("drain", "--wait")
	waiter.stderrHas(t, "0 unfinished, 1 cleanup receipt(s) pending")
	waiter.stillRunning(t)
	if err := os.Remove(f.gate.ReceiptPath(999)); err != nil {
		t.Fatal(err)
	}
	waiter.wait(t, 0)
}

func TestRealDaemonStatusShowsAnInvalidDrainFile(t *testing.T) {
	f := daemonAcceptance(t)
	mustWrite(t, drainPath(f.gate), Object{"until": "garbage"})
	output := f.cli("status", "--json").wait(t, 0)
	if !strings.Contains(output, `"drainError"`) || strings.Contains(output, `"drainUntil"`) {
		t.Fatalf("status json: %s", output)
	}
	if line := f.cli("status").wait(t, 0); !strings.HasPrefix(line, "Queue running, intake closed by an invalid drain file: ") {
		t.Fatalf("status line: %q", line)
	}
}

// followFixture follows job 5 against a fake daemon whose reachability the test
// controls. The job runs until the test says otherwise.
type followFixture struct {
	g       *Gate
	down    atomic.Bool
	dropped atomic.Bool // mutations (cancel, kill) are lost while reads still answer
	sent    atomic.Bool // the daemon goes away at the first read after the interrupt
	cancels atomic.Int32
	signals chan os.Signal
	stderr  *lockedBuffer
	done    chan followResult
}

type followResult struct {
	code int
	err  error
}

func newFollowFixture(t *testing.T, drain bool) *followFixture {
	t.Helper()
	f := &followFixture{g: testGate(t), signals: make(chan os.Signal, 4), done: make(chan followResult, 1)}
	if drain {
		holdDrain(t, f.g)
	}
	value := strings.Repeat("5", 32)
	f.g.queueControl = func(operation string, payload Object) (Object, error) {
		if f.sent.Load() && len(f.signals) == 0 {
			f.down.Store(true)
		}
		if f.down.Load() {
			return nil, errQueueTransportUnavailable
		}
		switch operation {
		case "snapshot":
			return Object{"tasks": Object{"5": Object{"token": value, "status": Object{"Running": Object{}}}}}, nil
		case "cancel", "kill":
			if f.dropped.Load() {
				return nil, errQueueTransportUnavailable
			}
			f.cancels.Add(1)
			return Object{}, nil
		}
		return nil, errors.New("unexpected " + operation)
	}
	f.stderr = captureStderr(t)
	go func() {
		code, err := followWithToken(f.g, 5, f.signals, value)
		f.done <- followResult{code, err}
	}()
	return f
}

func (f *followFixture) result(t *testing.T) followResult {
	t.Helper()
	select {
	case got := <-f.done:
		return got
	case <-time.After(20 * time.Second):
		t.Fatal("the follow loop did not end")
		return followResult{}
	}
}

// During a drain, an interrupt that meets an unreachable daemon leaves the job
// queued at once, whether the daemon was already away or went away just after
// the interrupt; nothing is cancelled and no cancellation is recorded.
func TestFollowDetachesOnAnInterruptDuringADrainGap(t *testing.T) {
	for name, arrange := range map[string]func(*followFixture){
		"the daemon was away": func(f *followFixture) {
			f.down.Store(true)
			waitForText(t, f.stderr, "(drain active); still waiting for job 5")
			f.signals <- syscall.SIGTERM
		},
		"the daemon went away just after the interrupt": func(f *followFixture) {
			f.sent.Store(true)
			f.signals <- syscall.SIGTERM
		},
		"the daemon answers reads but loses the cancellation": func(f *followFixture) {
			f.dropped.Store(true)
			f.signals <- syscall.SIGTERM
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFollowFixture(t, true)
			arrange(f)
			if got := f.result(t); got.code != 128+int(syscall.SIGTERM) || got.err != nil {
				t.Fatalf("code=%d err=%v", got.code, got.err)
			}
			if name == "the daemon answers reads but loses the cancellation" {
				// A cancel was sent, so the job may be gone or stopping.
				waitForText(t, f.stderr, "detaching; job 5 may still be running; check agent-heavy status")
			} else {
				waitForText(t, f.stderr, "detaching; job 5 stays in the queue, use agent-heavy cancel 5")
			}
			if f.cancels.Load() != 0 {
				t.Fatal("the job was cancelled")
			}
			if _, err := os.Stat(filepath.Join(f.g.Cancelled, "5.json")); err == nil && name != "the daemon answers reads but loses the cancellation" {
				t.Fatal("a cancellation record was left for a daemon that never answered")
			}
		})
	}
}

// A drain-free outage keeps the ordinary behaviour: the first interrupt is held
// and announced, and the budget decides.
func TestFollowOutsideADrainHoldsTheInterruptAndSpendsTheBudget(t *testing.T) {
	shortened := queueOutageBound
	queueOutageBound = 300 * time.Millisecond
	t.Cleanup(func() { queueOutageBound = shortened })
	f := newFollowFixture(t, false)
	f.down.Store(true)
	waitForText(t, f.stderr, "queue daemon not responding; still waiting for job 5")
	f.signals <- syscall.SIGTERM
	got := f.result(t)
	if got.code != 125 || got.err == nil || !strings.Contains(got.err.Error(), "job 5 may still be running") {
		t.Fatalf("code=%d err=%v", got.code, got.err)
	}
	waitForText(t, f.stderr, "interrupt received; cancelling job 5 once the queue daemon answers")
}

// A drain does not spend the outage budget; once it is over the budget starts.
func TestFollowOutageBudgetStartsWhenTheDrainEnds(t *testing.T) {
	shortened := queueOutageBound
	queueOutageBound = 300 * time.Millisecond
	t.Cleanup(func() { queueOutageBound = shortened })
	f := newFollowFixture(t, true)
	f.down.Store(true)
	waitForText(t, f.stderr, "(drain active); still waiting for job 5")
	// Far beyond the shortened bound, and the run is still waiting.
	fixtureUntil(t, 10*time.Second, func() bool { return strings.Count(f.stderr.String(), "(drain active)") >= 1 })
	select {
	case got := <-f.done:
		t.Fatalf("the drain outage ended the run: %+v", got)
	case <-time.After(2 * queueOutageBound):
	}
	if err := os.Remove(drainPath(f.g)); err != nil {
		t.Fatal(err)
	}
	got := f.result(t)
	if got.code != 125 || got.err == nil || !strings.Contains(got.err.Error(), "Lost contact with the queue daemon") {
		t.Fatalf("code=%d err=%v", got.code, got.err)
	}
}

// A finished job whose cleanup waits for a daemon that is away during a drain:
// an interrupt ends the wait and the caller reports the job's own exit status
// (as the follow loop does for any unconfirmed cleanup), naming the cause.
func TestFollowReportsTheFinishedJobsStatusWhenInterruptedDuringADrain(t *testing.T) {
	g := testGate(t)
	holdDrain(t, g)
	value := strings.Repeat("8", 32)
	mustWrite(t, g.ReceiptPath(8), Object{"task_id": 8, "token": value})
	shortened := queueCleanupWindow
	queueCleanupWindow = time.Minute
	t.Cleanup(func() { queueCleanupWindow = shortened })
	g.queueControl = func(string, Object) (Object, error) {
		return Object{"tasks": Object{"8": Object{"token": value, "status": Object{"Done": Object{"result": Object{"Failed": 7}}}}}}, nil
	}
	stderr := captureStderr(t)
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	code, err := followWithToken(g, 8, signals, value)
	if code != 7 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	waitForText(t, stderr, "Detaching during a drain: job 8 finished")
	waitForText(t, stderr, "the job's own exit status is 7")
}

func TestRunInLaneOnDrainUncertainSubmitSaysSoAndKeepsItsFiles(t *testing.T) {
	r := &raceScenario{}
	g := r.gate(t)
	r.outcome = func(g *Gate, n int) error { return errQueueTransportUnavailable }
	stderr := captureStderr(t)
	type outcome struct {
		code int
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		code, err := RunInLaneOnDrain(g, []string{"/usr/bin/true"}, "", "", drainWait)
		done <- outcome{code, err}
	}()
	waitForText(t, stderr, "submission outcome unknown; retrying the same request when the queue returns")
	if strings.Contains(stderr.String(), "no job created yet") {
		t.Fatalf("an uncertain submit must not claim that no job exists: %q", stderr.String())
	}
	// The wait is running, so its signal handler is registered.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.code != 128+int(syscall.SIGTERM) || result.err != nil {
			t.Fatalf("code=%d err=%v", result.code, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the uncertain wait did not end on a signal")
	}
	if !strings.Contains(stderr.String(), "the job may still exist, check agent-heavy status") || len(leftovers(g)) == 0 {
		t.Fatalf("stderr=%q left=%v", stderr.String(), leftovers(g))
	}
}

func syscallTerm() os.Signal { return syscall.SIGTERM }

func TestDrainWaitRefusesToRunInsideAQueuedCommand(t *testing.T) {
	g := testGate(t)
	t.Setenv("AGENT_HEAVY_ACTIVE", token())
	code, err := DrainMain(g, []string{"--wait"})
	if code != 125 || err == nil || !strings.Contains(err.Error(), "wait for its own job") {
		t.Fatalf("drain --wait inside a queued command: %d %v", code, err)
	}
	if _, statErr := os.Lstat(drainPath(g)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused drain must not close intake: %v", statErr)
	}
	code, err = DrainMain(g, []string{"--cancel-running"})
	if code != 125 || err == nil || !strings.Contains(err.Error(), "cancel its own job") {
		t.Fatalf("drain --cancel-running inside a queued command: %d %v", code, err)
	}
	if _, statErr := os.Lstat(drainPath(g)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused drain must not close intake: %v", statErr)
	}
	// Without those flags nothing waits on or cancels the job, so it stays allowed.
	if code, err = DrainMain(g, nil); code != 0 || err != nil {
		t.Fatalf("drain without --wait: %d %v", code, err)
	}
}

func TestRealDaemonOnDrainDirectForwardsSIGQUIT(t *testing.T) {
	f := daemonAcceptance(t)
	f.cli("drain").wait(t, 0)
	// Go's default for SIGQUIT would end only the caller and strand the command.
	trapped := f.launchOnDrain("direct", "/bin/sh", "-c", `trap 'echo quit > quit; exit 9' QUIT; echo ready > ready; while :; do sleep 0.05; done`)
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("ready") })
	if err := trapped.command.Process.Signal(syscall.SIGQUIT); err != nil {
		t.Fatal(err)
	}
	trapped.wait(t, 9)
	if !f.fileExists("quit") {
		t.Fatal("the command never saw SIGQUIT")
	}
	// A command that does not handle it dies of it, and the caller reports that
	// the way it reports any signalled command.
	killed := f.launchOnDrain("direct", "/bin/sh", "-c", `echo ready2 > ready2; exec sleep 60`)
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("ready2") })
	if err := killed.command.Process.Signal(syscall.SIGQUIT); err != nil {
		t.Fatal(err)
	}
	killed.wait(t, 128+int(syscall.SIGQUIT))
}

func TestDrainNoticesRepeatAtTheConfiguredPeriod(t *testing.T) {
	shortened := drainNoticePeriod
	drainNoticePeriod = 50 * time.Millisecond
	t.Cleanup(func() { drainNoticePeriod = shortened })
	count := func(buffer *lockedBuffer, text string) int { return strings.Count(buffer.String(), text) }
	waiting := func(buffer *lockedBuffer, text string) {
		t.Helper()
		fixtureUntil(t, 10*time.Second, func() bool { return count(buffer, text) >= 2 })
	}
	t.Run("a held caller", func(t *testing.T) {
		g := testGate(t)
		mustWrite(t, drainPath(g), Object{"until": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
		stderr := captureStderr(t)
		signals := make(chan os.Signal, 1)
		done := make(chan int, 1)
		go func() {
			code, _, _ := waitForDrain(g, time.Now().Add(time.Hour), signals)
			done <- code
		}()
		waiting(stderr, "waiting to submit")
		if !strings.Contains(stderr.String(), "agent-heavy undrain") {
			t.Fatalf("the notice does not name undrain: %q", stderr.String())
		}
		signals <- syscallTerm()
		if code := <-done; code != 128+15 {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("an attached caller without a daemon", func(t *testing.T) {
		g := testGate(t)
		mustWrite(t, drainPath(g), Object{"until": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
		g.queueControl = func(string, Object) (Object, error) { return nil, errQueueTransportUnavailable }
		stderr := captureStderr(t)
		signals := make(chan os.Signal, 1)
		done := make(chan int, 1)
		go func() {
			code, _ := followWithToken(g, 4, signals, token())
			done <- code
		}()
		waiting(stderr, "(drain active); still waiting for job 4")
		signals <- syscallTerm()
		if code := <-done; code != 128+15 {
			t.Fatalf("exit %d", code)
		}
	})
}

func TestRetriedTokenKeepsItsJobAfterTheLaneIsRemoved(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1}`)})
	value := strings.Repeat("c", 32)
	task := Object{"token": value, "priority": 0, "lane": "gone", "created_at": queueNow(), "status": Object{"Queued": Object{}}, "logPath": filepath.Join(d.g.State, "logs", "7.log")}
	data, err := queueJSON(task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.store.db.Exec("INSERT INTO queue_jobs(id,token,task,attempt,phase) VALUES(7,?,?,'','queued')", value, data); err != nil {
		t.Fatal(err)
	}
	result, err := d.control("submit-lane", Object{"token": value, "lane": "gone"})
	if err != nil || mustInteger(result["id"]) != 7 {
		t.Fatalf("a retried token must return its original identity: %v %v", result, err)
	}
	if _, err = d.control("submit-lane", Object{"token": strings.Repeat("d", 32), "lane": "gone"}); !errors.Is(err, errQueueUnknownLane) {
		t.Fatalf("a new token for an unknown lane: %v", err)
	}
}

// Cancel waits for a finished job's cleanup without a signal channel. When the
// daemon goes away there, that wait stays bounded even though a drain holds, and
// it reports the cleanup as unconfirmed beside the job's own status, as it does
// without a drain.
func TestInterruptDetachesWhenCleanupOutlastsTheDaemon(t *testing.T) {
	for _, draining := range []bool{true, false} {
		t.Run(fmt.Sprint("draining=", draining), func(t *testing.T) {
			g := testGate(t)
			if draining {
				holdDrain(t, g)
			}
			value := token()
			mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": value})
			shortened := queueCleanupWindow
			queueCleanupWindow = 200 * time.Millisecond
			t.Cleanup(func() { queueCleanupWindow = shortened })
			var polled atomic.Bool
			g.queueControl = func(string, Object) (Object, error) {
				if polled.Swap(true) {
					return nil, errQueueTransportUnavailable
				}
				return Object{"tasks": Object{"7": Object{"token": value, "status": Object{"Done": Object{"result": "Success"}}}}}, nil
			}
			type result struct {
				seen *int
				err  error
			}
			done := make(chan result, 1)
			go func() {
				seen, err := cancelJob(g, 7, value, nil)
				done <- result{seen, err}
			}()
			select {
			case got := <-done:
				var unconfirmed *queueCleanupUnconfirmed
				if got.seen == nil || *got.seen != 0 || !errors.As(got.err, &unconfirmed) {
					t.Fatalf("seen=%v err=%v", got.seen, got.err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("cleanup wait did not end")
			}
		})
	}
}

// proxySubmit lets a test decide what each submit attempt does to the real daemon.
func proxySubmit(f *daemonFixture, submit func(attempt int, payload Object) (Object, error)) *Gate {
	proxy := *f.gate
	attempts := 0
	proxy.queueControl = func(operation string, payload Object) (Object, error) {
		if operation == "submit" {
			attempts++
			return submit(attempts, payload)
		}
		return f.gate.QueueControl(operation, payload)
	}
	return &proxy
}

func holdDrain(t *testing.T, g *Gate) {
	t.Helper()
	mustWrite(t, drainPath(g), Object{"until": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
}

func countedCommand(root string) []string {
	return []string{"/bin/sh", "-c", "echo run >> " + filepath.Join(root, "count") + "; exit 6"}
}

// The submit commits, its response is lost and a drain begins. Every policy must
// find the committed job through its token, so the command runs in it once.
func TestCommittedSubmitWithALostResponseAttachesOnce(t *testing.T) {
	for _, policy := range []string{drainWait, drainFail, drainDirect} {
		t.Run(policy, func(t *testing.T) {
			f := daemonAcceptance(t)
			g := proxySubmit(f, func(attempt int, payload Object) (Object, error) {
				result, err := f.gate.QueueControl("submit", payload)
				if attempt > 1 {
					return result, err
				}
				if err != nil {
					t.Errorf("commit: %v", err)
				}
				holdDrain(t, f.gate)
				return nil, errQueueTransportUnavailable
			})
			stderr := captureStderr(t)
			code, err := RunInLaneOnDrain(g, countedCommand(f.root), "", "", policy)
			if code != 6 || err != nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
			if got := readText(t, filepath.Join(f.root, "count")); got != "run\n" {
				t.Fatalf("executions: %q", got)
			}
			if ids := f.taskIDs(); len(ids) != 1 || strings.Contains(stderr.String(), "outside the queue") {
				t.Fatalf("jobs=%v stderr=%q", ids, stderr.String())
			}
		})
	}
}

// The daemon refuses a real submission and the caller waits: its files must
// survive the wait, because the same token is submitted after the drain.
func TestRefusedSubmitKeepsItsFilesForTheRetry(t *testing.T) {
	f := daemonAcceptance(t)
	g := proxySubmit(f, func(attempt int, payload Object) (Object, error) {
		if attempt == 1 {
			holdDrain(t, f.gate)
		}
		return f.gate.QueueControl("submit", payload)
	})
	stderr := captureStderr(t)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := RunInLaneOnDrain(g, countedCommand(f.root), "", "", drainWait)
		done <- result{code, err}
	}()
	waitForText(t, stderr, "no job created yet")
	if ids := f.taskIDs(); len(ids) != 0 {
		t.Fatalf("job during refusal: %v", ids)
	}
	f.cli("undrain").wait(t, 0)
	select {
	case got := <-done:
		if got.code != 6 || got.err != nil {
			t.Fatalf("code=%d err=%v stderr=%q", got.code, got.err, stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("caller did not finish after undrain")
	}
	if got := readText(t, filepath.Join(f.root, "count")); got != "run\n" {
		t.Fatalf("executions: %q", got)
	}
}

// A lost response that a refusal later proves uncommitted leaves nothing behind.
func TestLostThenRefusedDirectRunsOnceWithNoJob(t *testing.T) {
	f := daemonAcceptance(t)
	g := proxySubmit(f, func(attempt int, payload Object) (Object, error) {
		if attempt == 1 {
			holdDrain(t, f.gate)
			return nil, errQueueTransportUnavailable
		}
		return f.gate.QueueControl("submit", payload)
	})
	code, err := RunInLaneOnDrain(g, countedCommand(f.root), "", "", drainDirect)
	if code != 6 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if got := readText(t, filepath.Join(f.root, "count")); got != "run\n" || len(f.taskIDs()) != 0 || len(leftovers(f.gate)) != 0 {
		t.Fatalf("executions=%q jobs=%v left=%v", got, f.taskIDs(), leftovers(f.gate))
	}
}

// A refusal proves the lost attempt committed nothing, so a later definite answer
// may discard the files again.
func TestRefusalClearsAnEarlierLostResponse(t *testing.T) {
	f := daemonAcceptance(t)
	g := proxySubmit(f, func(attempt int, payload Object) (Object, error) {
		switch attempt {
		case 1:
			holdDrain(t, f.gate)
			return nil, errQueueTransportUnavailable
		case 2:
			return f.gate.QueueControl("submit", payload)
		}
		return nil, &queueAnsweredError{"refused"}
	})
	stderr := captureStderr(t)
	done := make(chan int, 1)
	go func() {
		code, _ := RunInLaneOnDrain(g, countedCommand(f.root), "", "", drainWait)
		done <- code
	}()
	waitForText(t, stderr, "no job created yet")
	if err := os.Remove(drainPath(f.gate)); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if left := leftovers(f.gate); code != 125 || len(left) != 0 {
			t.Fatalf("code=%d left=%v", code, left)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("caller did not finish after the drain ended")
	}
}

// A retried lease token finds its lease during a drain; only a new token is refused.
func TestRetriedLeaseTokenKeepsItsLeaseDuringADrain(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1}`)})
	payload := Object{"token": token(), "lane": "qa", "ownerPid": os.Getpid()}
	first, err := d.control("lease-acquire", payload)
	if err != nil {
		t.Fatal(err)
	}
	holdDrain(t, d.g)
	again, err := d.control("lease-acquire", payload)
	if err != nil || mustInteger(again["id"]) != mustInteger(first["id"]) {
		t.Fatalf("retry: %v %v", again, err)
	}
	if _, err := d.control("lease-acquire", Object{"token": token(), "lane": "qa", "ownerPid": os.Getpid()}); !errors.Is(err, errQueueDraining) {
		t.Fatalf("new token: %v", err)
	}
}

// A job that finishes while the daemon is away has its result in the queue
// store. An interrupted caller reports that result and its output, not the
// interrupt, and does not say the job stays in the queue.
func TestRealDaemonDrainInterruptReportsAJobThatFinishedDuringTheGap(t *testing.T) {
	f := daemonAcceptance(t)
	client := f.launch("/bin/sh", "-c", `printf x >> "$1"; while [ ! -f "$2" ]; do sleep .05; done; echo final-output; exit 23`, "fixture", filepath.Join(f.root, "attempts"), filepath.Join(f.root, "release"))
	fixtureUntil(t, 8*time.Second, func() bool { return f.fileExists("attempts") })
	id := f.taskIDs()[0]
	f.cli("drain").wait(t, 0)
	f.stopDaemon()
	client.stderrHas(t, "(drain active); still waiting for job")
	if err := os.WriteFile(filepath.Join(f.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureUntil(t, 8*time.Second, func() bool {
		s, err := openQueueStore(f.gate.State, false)
		if err != nil {
			return false
		}
		defer s.close()
		j, err := s.job(id)
		return err == nil && j.phase == "outcome" && j.outcome.Valid && j.outcome.Int64 == 23
	})
	if err := client.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	client.wait(t, 23)
	if !strings.Contains(client.out.String(), "final-output") || !strings.Contains(client.errors.String(), "finished with exit status 23 (read from the queue store)") || strings.Contains(client.errors.String(), "stays in the queue") {
		t.Fatalf("out=%q err=%q", client.out.String(), client.errors.String())
	}
}

// Two callers hit the same stalled daemon while a drain holds. The one that
// waits for the scheduling lock gives up on neither the lock nor the drain.
func TestWaitCallersStayHeldWhenAStalledCallerKeepsTheSchedulingLock(t *testing.T) {
	shortened := queueLockWait
	queueLockWait = 200 * time.Millisecond
	t.Cleanup(func() { queueLockWait = shortened })
	g := testGate(t)
	var mu sync.Mutex
	tokens := map[string]int{}
	stalled, release := make(chan struct{}), make(chan struct{})
	var snapshots atomic.Int32
	var stalls sync.Once
	g.queueControl = func(operation string, payload Object) (Object, error) {
		switch operation {
		case "snapshot":
			// The second caller's precheck is the last read before the drain begins.
			if snapshots.Add(1) == 2 {
				holdDrain(t, g)
			}
			mu.Lock()
			defer mu.Unlock()
			tasks := Object{}
			for value, id := range tokens {
				tasks[strconv.Itoa(id)] = Object{"token": value, "status": Object{"Done": Object{"result": "Success"}}}
			}
			return Object{"groups": Object{"default": Object{"status": "Running", "parallel_tasks": 1}}, "tasks": tasks}, nil
		case "submit":
			first := false
			stalls.Do(func() { first = true })
			if first {
				close(stalled)
				<-release
				return nil, errQueueTransportUnavailable
			}
			mu.Lock()
			defer mu.Unlock()
			value := text(payload["token"])
			if _, ok := tokens[value]; !ok {
				tokens[value] = len(tokens) + 1
			}
			return Object{"id": tokens[value]}, nil
		}
		return nil, errors.New("unexpected " + operation)
	}
	type result struct {
		code int
		err  error
	}
	results := make(chan result, 2)
	call := func() {
		gate := *g
		code, err := RunInLaneOnDrain(&gate, []string{"/usr/bin/true"}, "", "", drainWait)
		results <- result{code, err}
	}
	go call()
	<-stalled
	go call()
	// The second caller times out on the lock while the first still stalls.
	fixtureUntil(t, 10*time.Second, func() bool { return snapshots.Load() >= 2 })
	select {
	case got := <-results:
		t.Fatalf("a caller ended during the drain: %+v", got)
	case <-time.After(3 * queueLockWait):
	}
	close(release)
	for range 2 {
		select {
		case got := <-results:
			if got.code != 0 || got.err != nil {
				t.Fatalf("code=%d err=%v", got.code, got.err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a caller did not finish")
		}
	}
}

// waitCallerFake answers a wait caller's snapshots and follow reads, and hands
// every submit to the test.
func waitCallerFake(g *Gate, submit func(attempt int, value string) (Object, error)) {
	var mu sync.Mutex
	value, attempts := "", 0
	g.queueControl = func(operation string, payload Object) (Object, error) {
		mu.Lock()
		defer mu.Unlock()
		switch operation {
		case "snapshot":
			tasks := Object{}
			if value != "" {
				tasks["1"] = Object{"token": value, "status": Object{"Done": Object{"result": "Success"}}}
			}
			return Object{"groups": Object{"default": Object{"status": "Running", "parallel_tasks": 1}}, "tasks": tasks}, nil
		case "submit":
			attempts++
			result, err := submit(attempts, text(payload["token"]))
			if err == nil {
				value = text(payload["token"])
			}
			return result, err
		}
		return nil, errors.New("unexpected " + operation)
	}
}

func runWaitCaller(t *testing.T, g *Gate) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		code, err := RunInLaneOnDrain(g, []string{"/usr/bin/true"}, "", "", drainWait)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit status %d", code)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("caller did not finish")
	}
}

// A lost daemon is retried without keeping the scheduling lock, so another caller
// can take it between attempts.
func TestWaitCallerFreesTheLockBetweenLostAttempts(t *testing.T) {
	g := testGate(t)
	var attempts atomic.Int32
	acquiredAt := make(chan int32, 1)
	var other sync.Once
	waitCallerFake(g, func(attempt int, value string) (Object, error) {
		attempts.Store(int32(attempt))
		other.Do(func() {
			holdDrain(t, g)
			go func() {
				unlock, err := LockPrivate(g, "scheduling.lock")
				if err != nil {
					t.Error(err)
					acquiredAt <- 0
					return
				}
				acquiredAt <- attempts.Load()
				unlock()
			}()
		})
		if attempt < 3 {
			return nil, errQueueTransportUnavailable
		}
		return Object{"id": 1}, nil
	})
	runWaitCaller(t, g)
	if got := <-acquiredAt; got == 0 || got >= 3 {
		t.Fatalf("the lock was free only after attempt %d", got)
	}
}

// Only a lost daemon skips the immediate retries: one other failed call must not
// end a wait caller whose earlier attempt may have committed.
func TestWaitCallerRetriesAnAnsweredErrorDuringADrain(t *testing.T) {
	g := testGate(t)
	waitCallerFake(g, func(attempt int, value string) (Object, error) {
		switch attempt {
		case 1:
			holdDrain(t, g)
			return nil, errQueueTransportUnavailable
		case 2:
			return nil, &queueAnsweredError{"database is locked"}
		}
		return Object{"id": 1}, nil
	})
	runWaitCaller(t, g)
}

// A drain that begins while a caller waits for the lock still applies when the
// lock wait times out.
func TestWaitCallerHoldsWhenADrainBeginsDuringItsLockWait(t *testing.T) {
	shortened := queueLockWait
	queueLockWait = 2 * time.Second
	t.Cleanup(func() { queueLockWait = shortened })
	g := testGate(t)
	unlock, err := LockPrivate(g, "scheduling.lock")
	if err != nil {
		t.Fatal(err)
	}
	var released sync.Once
	release := func() { released.Do(unlock) }
	defer release()
	waitCallerFake(g, func(attempt int, value string) (Object, error) { return Object{"id": 1}, nil })
	// The caller's precheck is its last read before the lock wait, so the drain
	// begins once that wait is under way.
	fake := g.queueControl
	var begins sync.Once
	g.queueControl = func(operation string, payload Object) (Object, error) {
		if operation == "snapshot" {
			begins.Do(func() { time.AfterFunc(queueLockWait/2, func() { holdDrain(t, g) }) })
		}
		return fake(operation, payload)
	}
	done := make(chan error, 1)
	go func() {
		code, err := RunInLaneOnDrain(g, []string{"/usr/bin/true"}, "", "", drainWait)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit status %d", code)
		}
		done <- err
	}()
	// The first lock wait ends while the test still holds the lock.
	select {
	case err := <-done:
		t.Fatalf("caller ended during the drain: %v", err)
	case <-time.After(queueLockWait + time.Second):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("caller did not finish")
	}
}

// Queued work already cancelled is still reported when running jobs cannot be read.
func TestDrainCancelReportsWhenRunningJobsCannotBeRead(t *testing.T) {
	g := testGate(t)
	g.queueControl = func(operation string, payload Object) (Object, error) {
		if operation == "cancel-queued" {
			return Object{"cancelled": 3}, nil
		}
		return nil, errors.New("snapshot failed")
	}
	stderr := captureStderr(t)
	if err := cancelForDrain(g, drainOptions{cancelQueued: true, cancelRunning: true}); err == nil || !strings.Contains(err.Error(), "snapshot failed") {
		t.Fatalf("error: %v", err)
	}
	waitForText(t, stderr, "cancelled 3 queued and 0 running or held item(s); running jobs could not be read\n")
}

// One failing cancel must not leave the jobs after it running.
func TestDrainCancelRunningTriesEveryJobAndReportsFailures(t *testing.T) {
	g := testGate(t)
	running := map[int]bool{1: true, 2: true, 3: true}
	var mu sync.Mutex
	g.queueControl = func(operation string, payload Object) (Object, error) {
		mu.Lock()
		defer mu.Unlock()
		switch operation {
		case "snapshot":
			tasks := Object{}
			for id, live := range running {
				state := Object{"Done": Object{"result": "Success"}}
				if live {
					state = Object{"Running": Object{}}
				}
				tasks[strconv.Itoa(id)] = Object{"token": fmt.Sprintf("%032x", id), "status": state}
			}
			return Object{"groups": Object{"default": Object{"status": "Running", "parallel_tasks": 1}}, "tasks": tasks}, nil
		case "cancel", "kill":
			if mustInteger(payload["id"]) == 2 {
				return nil, &queueAnsweredError{"database is locked"}
			}
			running[mustInteger(payload["id"])] = false
			return Object{}, nil
		}
		return nil, errors.New("unexpected " + operation)
	}
	stderr := captureStderr(t)
	code, err := DrainMain(g, []string{"--cancel-running"})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "job 2") || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("code=%d err=%v", code, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if running[1] || running[3] || !running[2] {
		t.Fatalf("every job but the failing one must be cancelled: %v", running)
	}
	waitForText(t, stderr, "cancelled 0 queued and 2 running or held item(s); 1 could not be cancelled")
}

// A worker that exits after publishing its outcome can leave a descendant that
// ignores SIGINT. A signal that finds the group without a known member must not
// be sent, and must tell the caller to read the row again when the row moved.
func TestSignalOfAGroupWhoseWorkerFinishedReadsTheRowAgain(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1}`)})
	survivor := exec.Command("/bin/sleep", "60")
	survivor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := survivor.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = survivor.Process.Kill(); _ = survivor.Wait() })
	boot, err := BootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	// The recorded worker is gone; only an unrecorded descendant leads the group.
	worker := Object{"pgid": survivor.Process.Pid, "pid": survivor.Process.Pid + 1_000_000, "boot_uuid": boot, "members": []Object{{"pid": survivor.Process.Pid + 1_000_000, "birth": "gone"}}}
	attempt := strings.Repeat("a", 32)
	value := strings.Repeat("b", 32)
	task := Object{"token": value, "priority": 0, "created_at": queueNow(), "status": Object{"Running": Object{}}, "logPath": filepath.Join(d.g.State, "logs", "9.log")}
	data, err := queueJSON(task)
	if err != nil {
		t.Fatal(err)
	}
	workerData, err := queueJSON(worker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.store.db.Exec("INSERT INTO queue_jobs(id,token,task,attempt,phase,worker) VALUES(9,?,?,?,'running',?)", value, data, attempt, workerData); err != nil {
		t.Fatal(err)
	}
	stale, err := d.store.job(9)
	if err != nil {
		t.Fatal(err)
	}
	// The row has not moved: the verification failure stands, and nothing is sent.
	if err = d.signal(stale, syscall.SIGINT); err == nil || errors.Is(err, errQueueChanged) {
		t.Fatalf("an unmoved row must keep the original error: %v", err)
	}
	// Another attempt took the row while the signal was being prepared.
	if _, err = d.store.db.Exec("UPDATE queue_jobs SET attempt=? WHERE id=9", strings.Repeat("c", 32)); err != nil {
		t.Fatal(err)
	}
	if err = d.signal(stale, syscall.SIGINT); !errors.Is(err, errQueueChanged) {
		t.Fatalf("a moved row must be read again: %v", err)
	}
	// The same worker publishes its outcome instead: nothing is left to signal.
	if _, err = d.store.db.Exec("UPDATE queue_jobs SET attempt=?, phase='outcome', outcome=0 WHERE id=9", attempt); err != nil {
		t.Fatal(err)
	}
	if err = d.signal(stale, syscall.SIGINT); err != nil {
		t.Fatalf("a settled row has nothing to signal: %v", err)
	}
	// Signals are delivered in order, so a SIGINT sent above would end it first.
	if err = survivor.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err = survivor.Wait(); !errors.As(err, &exit) {
		t.Fatalf("survivor exit: %v", err)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGTERM {
		t.Fatalf("a signal was sent to a group that could not be verified: %v", exit)
	}
}
