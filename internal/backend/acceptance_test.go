package backend

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type daemonFixture struct {
	t           *testing.T
	root        string
	gate        *Gate
	environment []string
	daemon      *exec.Cmd
	log         *os.File
	clients     []*queueClient
}
type queueClient struct {
	command     *exec.Cmd
	out, errors bytes.Buffer
	done        chan error
}

func fixtureUntil(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatal("isolated queue condition did not become true before deadline")
}
func daemonAcceptance(t *testing.T) *daemonFixture {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("queue runtime uses macOS ownership APIs")
	}
	root, err := os.MkdirTemp("/tmp", "queue-go-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	g := testGate(t)
	g.State = filepath.Join(root, "state")
	g.Config = filepath.Join(root, "config.yml")
	g.Slot = filepath.Join(root, "slot")
	g.Jobs = filepath.Join(g.State, "jobs")
	g.Receipts = filepath.Join(g.State, "receipts")
	g.Metadata = filepath.Join(g.State, "metadata")
	g.Cancelled = filepath.Join(g.State, "cancelled")
	if err = g.Prepare(); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{}
	for key, value := range envMap() {
		if !strings.HasPrefix(key, "AGENT_HEAVY_") && !strings.HasPrefix(key, "PUEUE_") {
			environment[key] = value
		}
	}
	environment["AGENT_HEAVY_CONFIG"] = g.Config
	environment["AGENT_HEAVY_STATE"] = g.State
	environment["AGENT_HEAVY_LEGACY_SLOT"] = g.Slot
	environment["PATH"] = environment["PATH"] + ":" + ToolPath
	g.ControlEnv = environment
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(root, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command(executable, "daemon")
	daemon.Env = envList(environment)
	daemon.Stdout = log
	daemon.Stderr = log
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = daemon.Start(); err != nil {
		t.Fatal(err)
	}
	f := &daemonFixture{t: t, root: root, gate: g, environment: envList(environment), daemon: daemon, log: log}
	t.Cleanup(func() {
		snapshot, _ := g.Snapshot()
		for key, raw := range obj(snapshot["tasks"]) {
			id, _ := strconv.Atoi(key)
			name, _, _ := TaskStatus(obj(raw))
			if name != "Done" {
				_ = Cancel(g, id)
			}
		}
		for _, client := range f.clients {
			select {
			case <-client.done:
			case <-time.After(15 * time.Second):
				_ = client.command.Process.Kill()
				<-client.done
			}
		}
		receipts, _ := os.ReadDir(g.Receipts)
		for _, entry := range receipts {
			if id, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".json")); err == nil {
				_ = Cleanup(g, id)
			}
		}
		_ = f.daemon.Process.Signal(syscall.SIGTERM)
		ended := make(chan error, 1)
		go func() { ended <- f.daemon.Wait() }()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			f.daemon.Process.Kill()
			<-ended
		}
		log.Close()
		if t.Failed() {
			body, _ := os.ReadFile(filepath.Join(root, "daemon.log"))
			t.Logf("private daemon output: %.5000s", body)
		}
	})
	fixtureUntil(t, 8*time.Second, func() bool { _, err := g.Snapshot(); return err == nil })
	if _, err = g.QueueControl("limit", Object{"limit": 1}); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *daemonFixture) launch(args ...string) *queueClient {
	f.t.Helper()
	executable, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	client := &queueClient{done: make(chan error, 1)}
	client.command = exec.Command(executable, append([]string{"run", "--"}, args...)...)
	client.command.Env = f.environment
	client.command.Dir = f.root
	client.command.Stdout = &client.out
	client.command.Stderr = &client.errors
	client.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = client.command.Start(); err != nil {
		f.t.Fatal(err)
	}
	go func() { client.done <- client.command.Wait(); close(client.done) }()
	f.clients = append(f.clients, client)
	return client
}
func (c *queueClient) wait(t *testing.T, expected int) string {
	t.Helper()
	select {
	case err := <-c.done:
		code := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != expected {
			t.Fatalf("exit=%d want=%d; output=%q errors=%q", code, expected, c.out.String(), c.errors.String())
		}
		return c.out.String()
	case <-time.After(20 * time.Second):
		t.Fatal("private queue client did not finish")
		return ""
	}
}
func (f *daemonFixture) taskIDs() []int {
	f.t.Helper()
	snapshot, err := f.gate.Snapshot()
	if err != nil {
		f.t.Fatal(err)
	}
	ids := []int{}
	for key := range obj(snapshot["tasks"]) {
		id, _ := strconv.Atoi(key)
		ids = append(ids, id)
	}
	return ids
}
func (f *daemonFixture) waitJobs(n int) {
	f.t.Helper()
	fixtureUntil(f.t, 8*time.Second, func() bool { snapshot, err := f.gate.Snapshot(); return err == nil && len(obj(snapshot["tasks"])) == n })
}

func TestRealDaemonLiteralArgumentsEnvironmentAndExit(t *testing.T) {
	f := daemonAcceptance(t)
	f.environment = append(f.environment, "QUEUE_LITERAL=private-value", "GOMAXPROCS=1", "GOMEMLIMIT=256MiB", "GOGC=75")
	client := f.launch("/bin/sh", "-c", "printf '%s\\n' \"$PWD\" \"$QUEUE_LITERAL\" \"$1\" \"$GOMAXPROCS,$GOMEMLIMIT,$GOGC\"; exit 7", "fixture", "literal $HOME `id`; space")
	expected := f.root + "\nprivate-value\nliteral $HOME `id`; space\n1,256MiB,75\n"
	if output := client.wait(t, 7); output != expected {
		t.Fatalf("literal forwarding: %q", output)
	}
	fixtureUntil(t, 4*time.Second, func() bool { _, err := os.Stat(f.gate.Slot); return os.IsNotExist(err) })
}

func TestRealDaemonPreservesPATHResolvedArgvZero(t *testing.T) {
	f := daemonAcceptance(t)
	client := f.launch("sh", "-c", "printf '%s' \"$0\"")
	if output := client.wait(t, 0); output != "sh" {
		t.Fatalf("argv[0] rewritten: %q", output)
	}
}
func TestRealDaemonConcurrentSubmittersRemainExclusive(t *testing.T) {
	f := daemonAcceptance(t)
	clients := []*queueClient{}
	for i := 0; i < 8; i++ {
		clients = append(clients, f.launch("/bin/sh", "-c", "mkdir active || exit 91; sleep 0.08; rmdir active"))
	}
	for _, client := range clients {
		client.wait(t, 0)
	}
}
func TestRealDaemonLargeFinalOutputAndStdinEOF(t *testing.T) {
	f := daemonAcceptance(t)
	client := f.launch("/usr/bin/head", "-c", "150000", "/dev/zero")
	if output := client.wait(t, 0); len(output) != 150000 {
		t.Fatal("truncated final output", len(output))
	}
	client = f.launch("/bin/sh", "-c", "if read -r value; then echo unexpected; exit 90; else echo EOF; fi")
	if output := client.wait(t, 0); output != "EOF\n" {
		t.Fatal(output)
	}
}
func TestRealDaemonLegacyOwnerAndWaitingCancellation(t *testing.T) {
	f := daemonAcceptance(t)
	if err := os.Mkdir(f.gate.Slot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(f.gate.Slot, "owner")
	if err := os.WriteFile(owner, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := f.launch("/bin/sh", "-c", "echo ran > first-ran")
	second := f.launch("/bin/sh", "-c", "echo forbidden > second-ran")
	f.waitJobs(2)
	if err := second.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	second.wait(t, 143)
	if _, err := os.Stat(filepath.Join(f.root, "first-ran")); !os.IsNotExist(err) {
		t.Fatal("ran before external owner released")
	}
	if _, err := os.Stat(filepath.Join(f.root, "second-ran")); !os.IsNotExist(err) {
		t.Fatal("cancelled waiter executed")
	}
	body, _ := os.ReadFile(owner)
	if string(body) != "external" {
		t.Fatal("changed external owner")
	}
	os.Remove(owner)
	os.Remove(f.gate.Slot)
	first.wait(t, 0)
}
func TestRealDaemonCallerDeathDoesNotReleaseWork(t *testing.T) {
	f := daemonAcceptance(t)
	client := f.launch("/bin/sh", "-c", "echo started > started; sleep 0.5; echo done > done")
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(filepath.Join(f.root, "started")); return err == nil })
	if err := client.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-client.done
	if _, err := os.Stat(f.gate.Slot); err != nil {
		t.Fatal("caller death removed worker lease", err)
	}
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(filepath.Join(f.root, "done")); return err == nil })
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(f.gate.Slot); return os.IsNotExist(err) })
}
func TestRealDaemonCancellationRemovesDescendants(t *testing.T) {
	f := daemonAcceptance(t)
	client := f.launch("/bin/sh", "-c", "trap 'exit 130' INT HUP; sleep 60 & child=$!; echo $child > child; wait")
	var child int
	fixtureUntil(t, 8*time.Second, func() bool {
		body, err := os.ReadFile(filepath.Join(f.root, "child"))
		if err != nil {
			return false
		}
		child, _ = strconv.Atoi(strings.TrimSpace(string(body)))
		return child > 1
	})
	ids := f.taskIDs()
	if len(ids) != 1 {
		t.Fatal(ids)
	}
	if err := Cancel(f.gate, ids[0]); err != nil {
		t.Fatal(err)
	}
	client.wait(t, 130)
	fixtureUntil(t, 4*time.Second, func() bool {
		cmd := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(child))
		out, _ := cmd.Output()
		state := strings.TrimSpace(string(out))
		return state == "" || strings.HasPrefix(state, "Z")
	})
}
func TestRealDaemonBackgroundChildCannotOutliveLease(t *testing.T) {
	f := daemonAcceptance(t)
	client := f.launch("/bin/sh", "-c", "sleep 60 & echo $! > background; echo final")
	if output := client.wait(t, 0); output != "final\n" {
		t.Fatal(output)
	}
	body, err := os.ReadFile(filepath.Join(f.root, "background"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(body)))
	cmd := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid))
	out, _ := cmd.Output()
	state := strings.TrimSpace(string(out))
	if state != "" && !strings.HasPrefix(state, "Z") {
		t.Fatal("background child survived released lease", pid, state)
	}
}
func TestRealDaemonLimitAndStartOverrideIsolation(t *testing.T) {
	f := daemonAcceptance(t)
	first := f.launch("/bin/sh", "-c", "echo started > first; while [ ! -f release ]; do sleep 0.1; done")
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(filepath.Join(f.root, "first")); return err == nil })
	second := f.launch("/bin/sh", "-c", "trap 'exit 130' INT HUP; echo started > second; while [ ! -f release ]; do sleep 0.1; done")
	f.waitJobs(2)
	third := f.launch("/bin/sh", "-c", "echo started > third")
	f.waitJobs(3)
	var secondID, thirdID int
	snapshot, err := f.gate.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for key := range obj(snapshot["tasks"]) {
		id, _ := strconv.Atoi(key)
		if id > thirdID {
			thirdID = id
		}
	}
	for key := range obj(snapshot["tasks"]) {
		id, _ := strconv.Atoi(key)
		if id != thirdID && id > secondID {
			secondID = id
		}
	}
	if _, err = Reorder(f.gate, thirdID, "next", 0); err != nil {
		t.Fatal(err)
	}
	if err = StartJob(f.gate, secondID); err != nil {
		t.Fatal(err)
	}
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(filepath.Join(f.root, "second")); return err == nil })
	if err = Cancel(f.gate, secondID); err != nil {
		t.Fatal(err)
	}
	second.wait(t, 130)
	if _, err = os.Stat(filepath.Join(f.root, "third")); !os.IsNotExist(err) {
		t.Fatal("start override changed normal concurrency")
	}
	if err = SetLimit(f.gate, 2); err != nil {
		t.Fatal(err)
	}
	third.wait(t, 0)
	if err = SetLimit(f.gate, 1); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	first.wait(t, 0)
	limit, err := PersistedLimit(f.gate)
	if err != nil || limit != 1 {
		t.Fatal("limit not persisted", limit, err)
	}
}

func TestRealDaemonLimitPreservesPausedJobAndQueue(t *testing.T) {
	f := daemonAcceptance(t)
	client := f.launch("/bin/sh", "-c", "echo started > paused; while [ ! -f release ]; do sleep 0.1; done")
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(filepath.Join(f.root, "paused")); return err == nil })
	ids := f.taskIDs()
	if len(ids) != 1 {
		t.Fatal(ids)
	}
	id := ids[0]
	if _, err := f.gate.QueueControl("pause-job", Object{"id": id}); err != nil {
		t.Fatal(err)
	}
	fixtureUntil(t, 5*time.Second, func() bool {
		task, err := f.gate.Task(id)
		if err != nil {
			return false
		}
		name, _, err := TaskStatus(task)
		return err == nil && name == "Paused"
	})
	if _, err := f.gate.QueueControl("pause", nil); err != nil {
		t.Fatal(err)
	}
	if err := SetLimit(f.gate, 2); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.gate.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if obj(obj(snapshot["groups"])["default"])["status"] != "Paused" {
		t.Fatal("limit resumed paused queue")
	}
	name, _, err := TaskStatus(obj(obj(snapshot["tasks"])[strconv.Itoa(id)]))
	if err != nil || name != "Paused" {
		t.Fatal("limit resumed paused job", err)
	}
	if _, err = f.gate.QueueControl("resume-job", Object{"id": id}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	client.wait(t, 0)
}

func TestRealDaemonCrashRetainsOwnershipAndPausesRestart(t *testing.T) {
	f := daemonAcceptance(t)
	first := f.launch("/bin/sh", "-c", "echo started > crash-started; sleep 60")
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(filepath.Join(f.root, "crash-started")); return err == nil })
	f.waitJobs(1)
	firstID := f.taskIDs()[0]
	second := f.launch("/bin/sh", "-c", "echo forbidden > crash-forbidden")
	f.waitJobs(2)
	secondID := -1
	for _, id := range f.taskIDs() {
		if id != firstID {
			secondID = id
		}
	}
	if secondID < 0 {
		t.Fatal("missing queued fixture job")
	}
	fixtureUntil(t, 5*time.Second, func() bool {
		s, e := openQueueStore(f.gate.State, false)
		if e != nil {
			return false
		}
		defer s.close()
		j, e := s.job(secondID)
		return e == nil && j.phase == "queued"
	})
	if err := f.daemon.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := f.daemon.Wait(); err == nil {
		t.Fatal("expected killed daemon")
	}
	if _, err := os.Stat(f.gate.Slot); err != nil {
		t.Fatal("daemon death released live work", err)
	}
	// Keep the outage observable until the waiter reports backend failure. It
	// must not run its target or silently bypass the missing queue daemon.
	second.wait(t, 125)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	restarted := exec.Command(executable, "daemon")
	restarted.Env = f.environment
	restarted.Stdout = f.log
	restarted.Stderr = f.log
	restarted.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = restarted.Start(); err != nil {
		t.Fatal(err)
	}
	f.daemon = restarted
	fixtureUntil(t, 8*time.Second, func() bool { _, err := f.gate.Snapshot(); return err == nil })
	snapshot, err := f.gate.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if obj(obj(snapshot["groups"])["default"])["status"] != "Paused" {
		t.Fatal("restarted daemon did not hold queued work")
	}
	if _, err = os.Stat(filepath.Join(f.root, "crash-forbidden")); !os.IsNotExist(err) {
		t.Fatal("queued target ran after daemon crash")
	}
	if err = Cleanup(f.gate, firstID); err != nil {
		t.Fatal(err)
	}
	if err = Cancel(f.gate, secondID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.done:
	case <-time.After(15 * time.Second):
		t.Fatal("original caller did not reconcile daemon failure")
	}
}
