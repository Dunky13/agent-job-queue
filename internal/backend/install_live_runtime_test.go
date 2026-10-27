package backend

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The actual current Go executable supplies the daemon, worker, caller and
// capability protocol. Only code-signing/native service registration are
// injected. This exercises publication and worker outcome writes across the
// replacement of the actual app files in a temporary private home.
func TestInstallerManagedLiveUpgradeRetainsRunningAttempts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("managed service handover requires macOS process birth identities")
	}
	for _, failure := range []string{"", "queued", "lost commit acknowledgement", "unregister resources", "unregister login", "register resources"} {
		name := failure
		if name == "" {
			name = "complete"
		}
		t.Run(name, func(t *testing.T) { testInstallerManagedLiveUpgrade(t, failure) })
	}
}

func TestInstallerPackagedLiveUpgrade(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("AGENT_QUEUE_TEST_INSTALLER_PAYLOAD") == "" {
		t.Skip("requires a built macOS installer payload")
	}
	for _, failure := range []string{"", "queued", "register resources"} {
		t.Run("packaged-"+failure, func(t *testing.T) { testInstallerManagedLiveUpgrade(t, failure) })
	}
}

func testInstallerManagedLiveUpgrade(t *testing.T, failure string) {
	queued := failure == "queued"
	lostCommit := failure == "lost commit acknowledgement"
	if queued || lostCommit {
		failure = ""
	}
	m := managedTestFixture(t)
	i := m.installer
	// Darwin's Unix socket pathname budget is smaller than t.TempDir's full
	// descriptive path. Keep the private fixture home deliberately short.
	base, err := os.MkdirTemp("/tmp", "aq-live-")
	if err != nil {
		t.Fatal(err)
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
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
	i.path += ":/usr/bin:/bin"
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if payload := os.Getenv("AGENT_QUEUE_TEST_INSTALLER_PAYLOAD"); payload != "" {
		executable = filepath.Join(payload, "source/bin/agent-heavy")
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{
		filepath.Join(i.payload, "source/bin/agent-heavy"),
		filepath.Join(i.payload, "Agent Queue.app/Contents/MacOS/agent-heavy"),
	} {
		if err := os.WriteFile(destination, binary, 0700); err != nil {
			t.Fatal(err)
		}
	}
	managedPackagedRuntimeFixture(t, m)
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[1] == "handover-capabilities" {
			command := exec.CommandContext(ctx, args[0], args[1:]...)
			command.Env = i.commandEnvironment(args)
			return command.CombinedOutput()
		}
		return original(ctx, args)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := runPackagedSetup(ctx, m.installFixture, i.execute); err != nil {
		for _, name := range []string{"daemon.log", "scheduler.log", "readiness.json"} {
			data, _ := os.ReadFile(filepath.Join(i.home, name))
			t.Log(name, string(data))
		}
		t.Fatal("initial private managed installation", err)
	}
	cli := filepath.Join(i.app, "Contents/MacOS/agent-heavy")
	environment := i.commandEnvironment([]string{cli, "status", "--json"})
	gate := i.liveGate(ctx)
	if _, err := gate.QueueControl("limit", Object{"limit": 2}); err != nil {
		t.Fatal(err)
	}
	started, release, runs := filepath.Join(i.home, "started"), filepath.Join(i.home, "release"), filepath.Join(i.home, "attempts")
	var blockerDone chan error
	blockerRelease := filepath.Join(i.home, "blocker-release")
	if queued {
		if _, err := gate.QueueControl("limit", Object{"limit": 1}); err != nil {
			t.Fatal(err)
		}
		blockerStarted := filepath.Join(i.home, "blocker-started")
		blocker := exec.CommandContext(ctx, cli, "run", "--", "/bin/sh", "-c", `touch "$1"; while [ ! -f "$2" ]; do sleep .05; done`, "fixture", blockerStarted, blockerRelease)
		blocker.Env = environment
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
		fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(blockerStarted); return err == nil })
	}
	old := exec.CommandContext(ctx, cli, "run", "--label", "Active across upgrade", "--", "/bin/sh", "-c", `printf 'once\n' >> "$1"; touch "$2"; while [ ! -f "$3" ]; do sleep .05; done; printf 'original outcome\n'; exit 23`, "fixture", runs, started, release)
	old.Env = environment
	var output bytes.Buffer
	old.Stdout, old.Stderr = &output, &output
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- old.Wait() }()
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
		fixtureUntil(t, 8*time.Second, func() bool {
			value, err := gate.Snapshot()
			return err == nil && len(obj(value["tasks"])) == 2
		})
		if _, err := gate.QueueControl("pause", nil); err != nil {
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
		fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(started); return err == nil })
	}
	before, err := managedCaptureBundle(i.app)
	if err != nil {
		t.Fatal(err)
	}
	managedCandidate(t, m, "124")
	if lostCommit {
		i.queueControl = func(ctx context.Context, operation string, payload Object) (Object, error) {
			response, err := i.liveGate(ctx).QueueControl(operation, payload)
			if operation == "commit-handover" && err == nil {
				return nil, errors.New("simulated lost commit acknowledgement")
			}
			return response, err
		}
	}
	if failure != "" {
		m.nativeFailure = failure
		if _, err := runPackagedSetup(ctx, m.installFixture, i.repairAndExecute); err == nil {
			t.Fatal("injected native failure unexpectedly completed")
		}
		select {
		case err := <-finished:
			joined = true
			t.Fatalf("interrupted installation lost its accepted caller: %v: %s", err, output.String())
		default:
		}
		m.nativeFailure = ""
	}
	if _, err := runPackagedSetup(ctx, m.installFixture, i.repairAndExecute); err != nil {
		t.Fatal("compatible upgrade waited for or interrupted active work", err)
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
	if queued {
		if _, err := os.Stat(started); !os.IsNotExist(err) {
			t.Fatal("handover resumed a deliberately paused queue", err)
		}
		if _, err := gate.QueueControl("resume", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := gate.QueueControl("limit", Object{"limit": 2}); err != nil {
			t.Fatal(err)
		}
		fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(started); return err == nil })
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
